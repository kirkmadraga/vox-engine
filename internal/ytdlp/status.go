package ytdlp

import (
	"context"
	"sync"
	"time"
)

// Run is one finished yt-dlp run, for the owner's debug command.
type Run struct {
	At      time.Time     // when it finished
	Took    time.Duration // the run itself
	Extract time.Duration // downloads: metadata and YouTube's JS challenge (0 if unknown)
	Outcome string        // "ok" or a failure kind, e.g. "bot-check"
}

// Recent remembers the last download and the last search (share one between
// the Downloader and the Searcher). Nil-safe.
type Recent struct {
	mu               sync.Mutex
	download, search Run
}

func (r *Recent) setDownload(run Run) {
	if r != nil {
		r.mu.Lock()
		r.download = run
		r.mu.Unlock()
	}
}

func (r *Recent) setSearch(run Run) {
	if r != nil {
		r.mu.Lock()
		r.search = run
		r.mu.Unlock()
	}
}

// Last returns the last download and search (zero Runs if none yet).
func (r *Recent) Last() (download, search Run) {
	if r == nil {
		return Run{}, Run{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.download, r.search
}

// VersionCache remembers yt-dlp's version for TTL, so the owner's debug
// command answers at once: starting yt-dlp takes seconds on a slow CPU, and
// its version changes at most daily (when it updates itself, while the bot
// runs). A failure isn't remembered; the next call asks again.
type VersionCache struct {
	Ask func(context.Context) (string, error) // e.g. Downloader.Version
	TTL time.Duration                         // 0 = DefaultVersionTTL
	Now func() time.Time                      // nil = time.Now

	mu      sync.Mutex // held while asking, so callers share one run
	version string
	at      time.Time
}

// DefaultVersionTTL is how long VersionCache trusts what yt-dlp said.
const DefaultVersionTTL = 10 * time.Minute

// Version returns yt-dlp's version, asking it if the last answer is too old.
// If asking fails, it returns the error along with the last version it knew
// ("" if none), which is likely still right.
func (c *VersionCache) Version(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = DefaultVersionTTL
	}
	if c.version != "" && now().Sub(c.at) < ttl {
		return c.version, nil
	}
	v, err := c.Ask(ctx)
	if err != nil {
		return c.version, err
	}
	c.version, c.at = v, now()
	return v, nil
}

// SearchStatus is the searcher's live state.
type SearchStatus struct {
	Running, Waiting, Max int
}

// Status reports running and waiting searches.
func (s *Searcher) Status() SearchStatus {
	s.once.Do(s.initSem)
	return SearchStatus{Running: len(s.sem), Waiting: int(s.waiting.Load()), Max: cap(s.sem)}
}

// run returns the timer's spans as a Run.
func (t *stageTimer) run(err error) Run {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := Run{At: t.stop, Outcome: outcome(err)}
	if r.At.IsZero() { // failed before yt-dlp ran (e.g. an invalid ID): it ended now
		r.At = t.now()
	}
	if !t.start.IsZero() && !t.stop.IsZero() {
		r.Took = round(t.stop.Sub(t.start))
	}
	if !t.start.IsZero() && !t.meta.IsZero() {
		r.Extract = round(t.meta.Sub(t.start))
	}
	return r
}
