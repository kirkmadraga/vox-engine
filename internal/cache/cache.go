package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// Fetcher downloads a video's audio into dir (a fresh, empty directory owned
// by the call). ytdlp.Downloader implements it.
type Fetcher interface {
	Fetch(ctx context.Context, id, dir string) (ytdlp.Result, error)
}

const (
	ext       = ".opus"
	tmpPrefix = ".dl-" // download scratch directories inside the cache dir

	// DefaultPurgeInterval is how often the age limit is enforced.
	DefaultPurgeInterval = 15 * time.Minute
)

// Cache stores audio as <dir>/<videoID>.opus and records each file in an
// Index. A file only appears under its final name once it is complete, valid
// and indexed, so a partial download is never mistaken for a hit. Concurrent
// requests for the same ID share one download.
//
// Only files named like a video ID (<11 chars>.opus) are ever managed or
// deleted; anything else in the directory (e.g. the test tone) is left alone.
type Cache struct {
	dir      string
	fetcher  Fetcher
	validate func(path string) error
	index    Index
	logger   *slog.Logger
	base     context.Context // downloads outlive the request that started them, not the bot
	slots    chan struct{}   // limits concurrent downloads
	maxBytes int64
	maxAge   time.Duration
	inUse    func() map[string]bool
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error

	// Downloads start at least minInterval apart, to stay polite to YouTube.
	minInterval time.Duration
	startMu     sync.Mutex
	lastStart   time.Time

	mu       sync.Mutex
	inflight map[string]*download

	purgeMu sync.Mutex // one purge at a time
	wg      sync.WaitGroup
}

type download struct {
	done  chan struct{}
	entry Entry
	err   error
}

// Options configures a Cache.
type Options struct {
	Fetcher       Fetcher
	Index         Index                                            // required
	Validate      func(path string) error                          // checks a downloaded file before it is admitted; may be nil
	MaxDownloads  int                                              // concurrent downloads; <1 means 1
	MaxBytes      int64                                            // total size cap; 0 = none
	MaxAge        time.Duration                                    // delete entries unused for this long; 0 = never
	InUse         func() map[string]bool                           // video IDs queued or playing; never purged. May be nil.
	PurgeInterval time.Duration                                    // how often MaxAge is enforced; 0 = DefaultPurgeInterval
	Now           func() time.Time                                 // nil = time.Now
	MinInterval   time.Duration                                    // minimum time between download starts; 0 = none
	Sleep         func(ctx context.Context, d time.Duration) error // nil = real timer (tests inject a fake)
	Logger        *slog.Logger
}

// New prepares dir, reconciles it with the index, enforces the limits once,
// and (if MaxAge is set) keeps enforcing them in the background until base is
// cancelled. Call Wait after cancelling base.
//
// Reconciliation: scratch directories from a crash, legacy <id>.json title
// files, and <id>.opus files the index doesn't know are deleted; index records
// whose file is gone are dropped.
func New(base context.Context, dir string, opts Options) (*Cache, error) {
	if opts.Index == nil {
		return nil, errors.New("cache: Options.Index is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	c := &Cache{
		dir:      dir,
		fetcher:  opts.Fetcher,
		validate: opts.Validate,
		index:    opts.Index,
		logger:   opts.Logger,
		base:     base,
		slots:    make(chan struct{}, max(opts.MaxDownloads, 1)),
		maxBytes: opts.MaxBytes,
		maxAge:   opts.MaxAge,
		inUse:    opts.InUse,
		now:      opts.Now,
		sleep:    opts.Sleep,
		inflight: map[string]*download{},

		minInterval: opts.MinInterval,
	}
	if c.logger == nil {
		c.logger = slog.New(slog.DiscardHandler)
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if err := c.reconcile(base); err != nil {
		return nil, err
	}
	c.Purge(base)
	if c.maxAge > 0 {
		interval := cmp.Or(opts.PurgeInterval, DefaultPurgeInterval)
		c.wg.Add(1)
		go c.purgeLoop(interval)
	}
	return c, nil
}

// Wait blocks until the background purge loop has stopped (after base is cancelled).
func (c *Cache) Wait() { c.wg.Wait() }

func (c *Cache) purgeLoop(interval time.Duration) {
	defer c.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.Purge(c.base)
		case <-c.base.Done():
			return
		}
	}
}

// videoFile returns the video ID for a cache-managed file name like
// "<id>.opus", and ok=false for anything else.
func videoFile(name, suffix string) (string, bool) {
	id, found := strings.CutSuffix(name, suffix)
	return id, found && ytdlp.ValidID(id)
}

func (c *Cache) reconcile(ctx context.Context) error {
	records, err := c.index.All(ctx)
	if err != nil {
		return fmt.Errorf("read cache index: %w", err)
	}
	known := make(map[string]bool, len(records))
	for _, r := range records {
		known[r.ID] = true
	}

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return fmt.Errorf("read cache dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(c.dir, name)
		switch {
		case e.IsDir() && strings.HasPrefix(name, tmpPrefix):
			c.removeLogged(p, "stale download dir", os.RemoveAll)
		case e.IsDir():
			// not ours
		case isVideo(name, ".json"):
			c.removeLogged(p, "legacy title file", os.Remove)
		case isVideo(name, ext) && !known[strings.TrimSuffix(name, ext)]:
			c.removeLogged(p, "file not in cache index", os.Remove)
		}
	}
	for _, r := range records {
		if !c.Has(r.ID) {
			c.logger.Info("cache: dropping index entry with no file", "video", r.ID)
			if err := c.index.Delete(ctx, r.ID); err != nil {
				return fmt.Errorf("clean cache index: %w", err)
			}
		}
	}
	return nil
}

func isVideo(name, suffix string) bool { _, ok := videoFile(name, suffix); return ok }

func (c *Cache) removeLogged(path, why string, remove func(string) error) {
	if err := remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.logger.Warn("cache: could not remove "+why, "path", path, "err", err)
		return
	}
	c.logger.Info("cache: removed "+why, "path", path)
}

// Path is where id's audio lives once cached.
func (c *Cache) Path(id string) string { return filepath.Join(c.dir, id+ext) }

// Has reports whether id's audio file is present.
func (c *Cache) Has(id string) bool {
	info, err := os.Stat(c.Path(id))
	return err == nil && info.Mode().IsRegular()
}

// Entry is a cached track, ready to play.
type Entry struct {
	ID       string
	Path     string
	Title    string        // empty if unknown
	Duration time.Duration // 0 if unknown
}

func (c *Cache) entry(ctx context.Context, id string) Entry {
	e := Entry{ID: id, Path: c.Path(id)}
	if r, ok, err := c.index.Get(ctx, id); err == nil && ok {
		e.Title, e.Duration = r.Title, r.Duration
	}
	return e
}

// Get returns id's cached audio, downloading it if needed. hit is true when no
// download was necessary. If the caller's ctx ends first, Get returns
// ctx.Err() but the download keeps going for other waiters and future plays.
func (c *Cache) Get(ctx context.Context, id string) (e Entry, hit bool, err error) {
	if !ytdlp.ValidID(id) {
		return Entry{}, false, &ytdlp.Error{Kind: ytdlp.KindInvalid, Detail: fmt.Sprintf("invalid video id %q", id)}
	}
	if c.Has(id) {
		return c.entry(ctx, id), true, nil
	}

	c.mu.Lock()
	d, ok := c.inflight[id]
	if !ok {
		// Re-check under the lock: a download may have just finished.
		if c.Has(id) {
			c.mu.Unlock()
			return c.entry(ctx, id), true, nil
		}
		d = &download{done: make(chan struct{})}
		c.inflight[id] = d
		go c.run(id, d)
	}
	c.mu.Unlock()

	select {
	case <-d.done:
		return d.entry, false, d.err
	case <-ctx.Done():
		return Entry{}, false, ctx.Err()
	}
}

// MarkPlayed records that id started playing, which keeps it from expiring.
func (c *Cache) MarkPlayed(id string) {
	if err := c.index.MarkPlayed(c.base, id, c.now().UTC()); err != nil {
		c.logger.Warn("cache: could not record play", "video", id, "err", err)
	}
}

// run performs one download and publishes the result to all waiters.
func (c *Cache) run(id string, d *download) {
	d.entry, d.err = c.fetch(id)
	c.mu.Lock()
	delete(c.inflight, id) // failures are not cached: the next request retries
	c.mu.Unlock()
	close(d.done)
	if d.err == nil {
		c.Purge(c.base, id) // the cache just grew; never evict what was just fetched
	}
}

func (c *Cache) fetch(id string) (Entry, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-c.base.Done():
		return Entry{}, c.base.Err()
	}
	if err := c.waitTurn(id); err != nil {
		return Entry{}, err
	}

	tmp, err := os.MkdirTemp(c.dir, tmpPrefix+id+"-")
	if err != nil {
		return Entry{}, fmt.Errorf("create download dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	log := c.logger.With("video", id)
	log.Info("cache: downloading")
	res, err := c.fetcher.Fetch(c.base, id, tmp)
	if err != nil {
		log.Warn("cache: download failed", "err", err)
		return Entry{}, err
	}
	// Only trust the file name: the printed directory may be spelled differently
	// (e.g. Windows 8.3 short names), and the file must be inside tmp anyway.
	got := filepath.Join(tmp, filepath.Base(res.Path))
	info, err := os.Stat(got)
	if err != nil || !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("downloader reported %q, but no such file was written", filepath.Base(res.Path))
	}
	validateStart := c.now()
	if c.validate != nil {
		if err := c.validate(got); err != nil {
			log.Warn("cache: downloaded file rejected", "err", err)
			return Entry{}, fmt.Errorf("downloaded audio is unusable: %w", err)
		}
	}
	validated := c.now().Sub(validateStart).Round(time.Millisecond)

	// Index first, then move the file into place. A crash in between leaves an
	// index entry without a file, which the next startup drops. The reverse
	// order could leave a file the index doesn't know, which would be deleted.
	rec := Record{ID: id, Title: res.Title, Duration: res.Duration, Size: info.Size(), AddedAt: c.now().UTC()}
	if err := c.index.Put(c.base, rec); err != nil {
		return Entry{}, fmt.Errorf("record in cache index: %w", err)
	}
	// Same directory tree, so this is an atomic rename, not a copy. The
	// downloader has exited, so no handle is open on the file (Windows).
	if err := os.Rename(got, c.Path(id)); err != nil {
		_ = c.index.Delete(c.base, id)
		return Entry{}, fmt.Errorf("move into cache: %w", err)
	}
	log.Info("cache: stored", "title", res.Title, "bytes", rec.Size, "validate", validated)
	return Entry{ID: id, Path: c.Path(id), Title: res.Title, Duration: res.Duration}, nil
}

// Purge enforces MaxAge and MaxBytes. Entries that are queued, playing,
// downloading, or listed in protect are never removed. Expired entries go
// first; then, while over the size cap, the least recently used.
func (c *Cache) Purge(ctx context.Context, protect ...string) {
	if c.maxAge <= 0 && c.maxBytes <= 0 {
		return
	}
	c.purgeMu.Lock()
	defer c.purgeMu.Unlock()

	records, err := c.index.All(ctx)
	if err != nil {
		c.logger.Error("cache: purge could not read index", "err", err)
		return
	}
	keep := map[string]bool{}
	if c.inUse != nil {
		for id := range c.inUse() {
			keep[id] = true
		}
	}
	for _, id := range protect {
		keep[id] = true
	}
	c.mu.Lock()
	for id := range c.inflight {
		keep[id] = true
	}
	c.mu.Unlock()

	now := c.now()
	var survivors []Record
	var total int64
	for _, r := range records {
		if c.maxAge > 0 && !keep[r.ID] && now.Sub(r.LastUsed()) > c.maxAge {
			c.evict(ctx, r, "unused for longer than "+c.maxAge.String())
			continue
		}
		survivors = append(survivors, r)
		total += r.Size
	}
	if c.maxBytes > 0 && total > c.maxBytes {
		slices.SortStableFunc(survivors, func(a, b Record) int { return a.LastUsed().Compare(b.LastUsed()) })
		for _, r := range survivors {
			if total <= c.maxBytes {
				break
			}
			if keep[r.ID] {
				continue
			}
			c.evict(ctx, r, "over the size cap")
			total -= r.Size
		}
		if total > c.maxBytes {
			c.logger.Warn("cache: still over the size cap; the rest is queued or playing", "bytes", total, "cap", c.maxBytes)
		}
	}
}

func (c *Cache) evict(ctx context.Context, r Record, why string) {
	if err := os.Remove(c.Path(r.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.logger.Warn("cache: could not delete", "video", r.ID, "err", err)
		return
	}
	if err := c.index.Delete(ctx, r.ID); err != nil {
		c.logger.Warn("cache: could not drop index entry", "video", r.ID, "err", err)
	}
	c.logger.Info("cache: evicted", "video", r.ID, "title", r.Title, "bytes", r.Size, "reason", why)
}

// Size returns the total bytes of indexed audio.
func (c *Cache) Size(ctx context.Context) (int64, error) {
	records, err := c.index.All(ctx)
	var total int64
	for _, r := range records {
		total += r.Size
	}
	return total, err
}

// waitTurn delays a download until at least minInterval after the previous
// one started, so a burst of requests (e.g. a freshly filled queue) reaches
// YouTube one at a time, spread out. Queued tracks just wait their turn.
func (c *Cache) waitTurn(id string) error {
	if c.minInterval <= 0 {
		return nil
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if !c.lastStart.IsZero() {
		if wait := c.lastStart.Add(c.minInterval).Sub(c.now()); wait > 0 {
			c.logger.Info("cache: waiting before next download", "video", id, "wait", wait.Round(time.Second))
			if err := c.sleep(c.base, wait); err != nil {
				return err
			}
		}
	}
	c.lastStart = c.now()
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
