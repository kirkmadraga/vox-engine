package ytdlp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoResults means a search found no videos.
var ErrNoResults = errors.New("no YouTube results")

// DefaultSearchTimeout bounds one search.
const DefaultSearchTimeout = 30 * time.Second

// MaxQueryLen caps the search text passed to yt-dlp.
const MaxQueryLen = 200

// MaxResults caps how many results one search returns.
const MaxResults = 10

// Hit is a search result.
type Hit struct {
	ID       string
	Title    string        // may be empty
	Duration time.Duration // 0 if unknown (always 0 for YouTube Music results)
	Channel  string        // may be empty (always empty for YouTube Music results)
	Verified bool          // the channel is verified
}

// Searcher finds YouTube videos for some words using yt-dlp. It only lists
// results (no download). At most MaxConcurrent searches run at once. Use it by
// pointer.
type Searcher struct {
	Runner        Runner
	Path          string           // yt-dlp executable
	JSRuntime     string           // optional: passed as --js-runtimes
	Cookies       string           // optional: Netscape cookies.txt; each search gets a private copy, never saved back
	Timeout       time.Duration    // 0 = DefaultSearchTimeout
	Lock          sync.Locker      // optional: guards Cookies while it's copied; share with Downloader.Lock
	MaxConcurrent int              // searches at the same time; <1 = 1
	Logger        *slog.Logger     // optional: timing per search
	Now           func() time.Time // nil = time.Now; tests replace it
	Recent        *Recent          // optional: remembers the last search

	once    sync.Once
	sem     chan struct{} // one token per running search
	waiting atomic.Int32  // searches waiting for a slot
}

func (s *Searcher) initSem() { s.sem = make(chan struct{}, max(s.MaxConcurrent, 1)) }

// acquire waits for a free search slot; the returned func releases it. It
// gives up if ctx ends first, so a request that has run out of time doesn't
// take a slot only to fail.
func (s *Searcher) acquire(ctx context.Context) (release func(), err error) {
	s.once.Do(s.initSem)
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// searchFields are printed for every result, as one JSON object per line.
const searchFields = "%(.{id,duration,title,channel,channel_is_verified})j"

// MusicSearchURL is the YouTube Music "Songs" search for query. The query is
// URL-encoded, so it can't be read as an option or change the URL.
func MusicSearchURL(query string) string {
	return "https://music.youtube.com/search?q=" + url.QueryEscape(cleanQuery(query)) + "#songs"
}

// SearchArgs returns the yt-dlp arguments for the first n YouTube results for
// query, with the configured cookies file. The query is only ever passed after
// the "ytsearchN:" prefix, so it can't be read as an option.
func (s *Searcher) SearchArgs(query string, n int) []string {
	return s.args(s.Cookies, fmt.Sprintf("ytsearch%d:%s", n, cleanQuery(query)))
}

// MusicSearchArgs returns the yt-dlp arguments for the first n YouTube Music
// song results for query, with the configured cookies file.
func (s *Searcher) MusicSearchArgs(query string, n int) []string {
	return s.args(s.Cookies, "--playlist-items", fmt.Sprintf("1:%d", n), MusicSearchURL(query))
}

func (s *Searcher) args(cookies string, target ...string) []string {
	args := []string{
		"--flat-playlist",
		"--no-warnings",
		"--socket-timeout", "20",
		"--print", metaMarker + searchFields,
	}
	if s.JSRuntime != "" {
		args = append(args, "--js-runtimes", s.JSRuntime)
	}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	return append(args, target...)
}

// cleanQuery flattens whitespace (including newlines) and caps the length.
func cleanQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > MaxQueryLen {
		q = string(r[:MaxQueryLen])
	}
	return q
}

// SearchN returns up to n (1..MaxResults) YouTube videos for query, in
// YouTube's order, or ErrNoResults.
func (s *Searcher) SearchN(ctx context.Context, query string, n int) ([]Hit, error) {
	n = min(max(n, 1), MaxResults)
	return s.search(ctx, "youtube", query, n, func(cookies string) []string {
		return s.args(cookies, fmt.Sprintf("ytsearch%d:%s", n, cleanQuery(query)))
	})
}

// SearchMusic returns up to n (1..MaxResults) YouTube Music song results for
// query, in YouTube Music's order, or ErrNoResults. Results carry only an ID
// and a title.
func (s *Searcher) SearchMusic(ctx context.Context, query string, n int) ([]Hit, error) {
	n = min(max(n, 1), MaxResults)
	return s.search(ctx, "youtube-music", query, n, func(cookies string) []string {
		return s.args(cookies, "--playlist-items", fmt.Sprintf("1:%d", n), MusicSearchURL(query))
	})
}

// search runs yt-dlp with args(cookies) and parses up to n results. kind
// names the search in the timing log.
func (s *Searcher) search(ctx context.Context, kind, query string, n int, args func(cookies string) []string) (hits []Hit, err error) {
	if cleanQuery(query) == "" {
		return nil, ErrNoResults
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	asked := now()
	release, err := s.acquire(ctx)
	if err != nil { // no yt-dlp run, so nothing for the timing log or debug ytdlp
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Kind: KindTimeout, Detail: fmt.Sprintf("gave up waiting for a search slot after %v", round(now().Sub(asked)))}
		}
		return nil, err
	}
	defer release()
	started := now()
	defer func() {
		s.Recent.setSearch(Run{At: now(), Took: round(now().Sub(started)), Outcome: searchOutcome(err)})
	}()
	if s.Logger != nil {
		defer func() {
			s.Logger.Info("ytdlp: search timing", "kind", kind, "results", len(hits),
				"took", round(now().Sub(started)), "waited", round(started.Sub(asked)), "outcome", searchOutcome(err))
		}()
	}
	var cookies string // this search's private copy, thrown away afterwards
	if s.Cookies != "" {
		c, err := checkoutCookies(s.Cookies, s.Lock)
		if err != nil {
			return nil, &Error{Kind: KindFailed, Detail: err.Error()}
		}
		defer os.Remove(c)
		cookies = c
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultSearchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, err := s.Runner.Run(ctx, s.Path, args(cookies))
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &Error{Kind: KindTimeout, Detail: fmt.Sprintf("yt-dlp search timed out after %v", timeout)}
		}
		return nil, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &Error{Kind: KindMissingTool, Detail: err.Error()}
		}
		return nil, classify(string(stderr), err)
	}
	return parseResults(string(stdout), n)
}

// parseResults reads "VOXMETA <json>" lines. Results that aren't videos (e.g.
// channels) are skipped; if nothing valid remains, that's an error.
func parseResults(stdout string, n int) ([]Hit, error) {
	var hits []Hit
	var bad []string
	for _, line := range strings.Split(stdout, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), metaMarker)
		if !ok {
			continue
		}
		var r struct {
			ID       string  `json:"id"`
			Duration float64 `json:"duration"`
			Title    string  `json:"title"`
			Channel  string  `json:"channel"`
			Verified bool    `json:"channel_is_verified"`
		}
		if err := json.Unmarshal([]byte(rest), &r); err != nil || !ValidID(r.ID) {
			bad = append(bad, truncate(rest, 60))
			continue
		}
		h := Hit{ID: r.ID, Title: strings.TrimSpace(r.Title), Channel: strings.TrimSpace(r.Channel), Verified: r.Verified}
		if r.Duration > 0 {
			h.Duration = time.Duration(r.Duration * float64(time.Second))
		}
		hits = append(hits, h)
		if len(hits) == n {
			break
		}
	}
	switch {
	case len(hits) > 0:
		return hits, nil
	case len(bad) > 0:
		return nil, &Error{Kind: KindFailed, Detail: fmt.Sprintf("search returned no valid videos: %q", bad)}
	}
	return nil, ErrNoResults
}

// searchOutcome is outcome, with "no-results" for an empty search.
func searchOutcome(err error) string {
	if errors.Is(err, ErrNoResults) {
		return "no-results"
	}
	return outcome(err)
}
