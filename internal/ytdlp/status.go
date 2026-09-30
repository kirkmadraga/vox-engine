package ytdlp

import (
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
	if !t.start.IsZero() && !t.stop.IsZero() {
		r.Took = round(t.stop.Sub(t.start))
	}
	if !t.start.IsZero() && !t.meta.IsZero() {
		r.Extract = round(t.meta.Sub(t.start))
	}
	return r
}
