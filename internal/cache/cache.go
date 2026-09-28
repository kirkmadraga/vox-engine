package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bot/internal/ytdlp"
)

// Fetcher downloads a video's audio into dir (a fresh, empty directory owned
// by the call). ytdlp.Downloader implements it.
type Fetcher interface {
	Fetch(ctx context.Context, id, dir string) (ytdlp.Result, error)
}

const (
	ext       = ".opus"
	tmpPrefix = ".dl-" // download scratch directories inside the cache dir
)

// Cache stores audio as <dir>/<videoID>.opus. A file only appears under its
// final name once it is complete and valid, so a partial download is never
// mistaken for a hit. Concurrent requests for the same ID share one download.
type Cache struct {
	dir      string
	fetcher  Fetcher
	validate func(path string) error
	logger   *slog.Logger
	base     context.Context // downloads outlive the request that started them, not the bot
	slots    chan struct{}   // limits concurrent downloads

	mu       sync.Mutex
	inflight map[string]*download
}

type download struct {
	done  chan struct{}
	entry Entry
	err   error
}

// Options configures a Cache.
type Options struct {
	Fetcher      Fetcher
	Validate     func(path string) error // checks a downloaded file before it is admitted; may be nil
	MaxDownloads int                     // concurrent downloads; <1 means 1
	Logger       *slog.Logger
}

// New prepares dir (creating it, and removing scratch directories left by a
// crash) and returns a Cache. base bounds all downloads: cancel it on shutdown.
func New(base context.Context, dir string, opts Options) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	c := &Cache{
		dir:      dir,
		fetcher:  opts.Fetcher,
		validate: opts.Validate,
		logger:   opts.Logger,
		base:     base,
		slots:    make(chan struct{}, max(opts.MaxDownloads, 1)),
		inflight: map[string]*download{},
	}
	if c.logger == nil {
		c.logger = slog.New(slog.DiscardHandler)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read cache dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				c.logger.Warn("cache: could not remove stale download dir", "dir", e.Name(), "err", err)
			}
		}
	}
	return c, nil
}

// Path is where id's audio lives once cached.
func (c *Cache) Path(id string) string { return filepath.Join(c.dir, id+ext) }

// Has reports whether id is already cached.
func (c *Cache) Has(id string) bool {
	info, err := os.Stat(c.Path(id))
	return err == nil && info.Mode().IsRegular()
}

// Entry is a cached track.
type Entry struct {
	ID       string
	Path     string
	Title    string        // empty if unknown
	Duration time.Duration // 0 if unknown
}

// meta is the <id>.json sidecar holding what yt-dlp told us about a video.
type meta struct {
	Title           string  `json:"title"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func (c *Cache) metaPath(id string) string { return filepath.Join(c.dir, id+".json") }

// entry builds the Entry for a cached id, reading its sidecar if present.
func (c *Cache) entry(id string) Entry {
	e := Entry{ID: id, Path: c.Path(id)}
	if data, err := os.ReadFile(c.metaPath(id)); err == nil {
		var m meta
		if json.Unmarshal(data, &m) == nil {
			e.Title = m.Title
			e.Duration = time.Duration(m.DurationSeconds * float64(time.Second))
		}
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
		return c.entry(id), true, nil
	}

	c.mu.Lock()
	d, ok := c.inflight[id]
	if !ok {
		// Re-check under the lock: a download may have just finished.
		if c.Has(id) {
			c.mu.Unlock()
			return c.entry(id), true, nil
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

// run performs one download and publishes the result to all waiters.
func (c *Cache) run(id string, d *download) {
	d.entry, d.err = c.fetch(id)
	c.mu.Lock()
	delete(c.inflight, id) // failures are not cached: the next request retries
	c.mu.Unlock()
	close(d.done)
}

func (c *Cache) fetch(id string) (Entry, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-c.base.Done():
		return Entry{}, c.base.Err()
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
	if info, err := os.Stat(got); err != nil || !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("downloader reported %q, but no such file was written", filepath.Base(res.Path))
	}
	if c.validate != nil {
		if err := c.validate(got); err != nil {
			log.Warn("cache: downloaded file rejected", "err", err)
			return Entry{}, fmt.Errorf("downloaded audio is unusable: %w", err)
		}
	}

	// Metadata goes in first: the audio file's appearance is what makes a hit,
	// so a hit always has its sidecar. A failed sidecar write only loses the title.
	data, _ := json.Marshal(meta{Title: res.Title, DurationSeconds: res.Duration.Seconds()})
	if err := writeFileAtomic(tmp, c.metaPath(id), data); err != nil {
		log.Warn("cache: could not save metadata", "err", err)
	}
	// Same directory tree, so this is an atomic rename, not a copy. The
	// downloader has exited, so no handle is open on the file (Windows).
	if err := os.Rename(got, c.Path(id)); err != nil {
		return Entry{}, fmt.Errorf("move into cache: %w", err)
	}
	size, _ := c.Size()
	log.Info("cache: stored", "title", res.Title, "cache_bytes", size)
	return Entry{ID: id, Path: c.Path(id), Title: res.Title, Duration: res.Duration}, nil
}

// writeFileAtomic writes data to a file in scratch, closes it, then renames it to dst.
func writeFileAtomic(scratch, dst string, data []byte) error {
	tmp := filepath.Join(scratch, filepath.Base(dst)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Size returns the total bytes of cached audio files.
func (c *Cache) Size() (int64, error) {
	var total int64
	err := filepath.WalkDir(c.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && p != c.dir {
			return filepath.SkipDir // skip scratch dirs
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ext) {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}
