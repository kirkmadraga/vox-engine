package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	Duration time.Duration // 0 if unknown
}

// Searcher finds YouTube videos for some words using yt-dlp. It only lists
// results (no download). Searches run one at a time. Use it by pointer.
type Searcher struct {
	Runner    Runner
	Path      string        // yt-dlp executable
	JSRuntime string        // optional: passed as --js-runtimes
	Cookies   string        // optional: Netscape cookies.txt; each search gets a private copy, never saved back
	Timeout   time.Duration // 0 = DefaultSearchTimeout
	Lock      sync.Locker   // optional: guards Cookies while it's copied; share with Downloader.Lock

	mu sync.Mutex // searches run one at a time
}

// SearchArgs returns the yt-dlp arguments for the first n results for query,
// with the configured cookies file. The query is only ever passed after the
// "ytsearchN:" prefix, so it can't be read as an option.
func (s *Searcher) SearchArgs(query string, n int) []string {
	return s.searchArgs(query, n, s.Cookies)
}

func (s *Searcher) searchArgs(query string, n int, cookies string) []string {
	args := []string{
		"--flat-playlist",
		"--no-warnings",
		"--socket-timeout", "20",
		"--print", metaMarker + "%(id)s %(duration)s %(title)s",
	}
	if s.JSRuntime != "" {
		args = append(args, "--js-runtimes", s.JSRuntime)
	}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	return append(args, fmt.Sprintf("ytsearch%d:%s", n, cleanQuery(query)))
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
	if cleanQuery(query) == "" {
		return nil, ErrNoResults
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

	stdout, stderr, err := s.Runner.Run(ctx, s.Path, s.searchArgs(query, n, cookies))
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

	// VOXMETA <id> <duration seconds or NA> <title, may contain spaces>
	var hits []Hit
	var bad []string
	for _, line := range strings.Split(string(stdout), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), metaMarker)
		if !ok {
			continue
		}
		id, rest, _ := strings.Cut(rest, " ")
		duration, title, _ := strings.Cut(rest, " ")
		if !ValidID(id) {
			bad = append(bad, id) // not a video (or garbage): skip it
			continue
		}
		h := Hit{ID: id, Title: strings.TrimSpace(title)}
		if secs, err := strconv.ParseFloat(duration, 64); err == nil && secs > 0 {
			h.Duration = time.Duration(secs * float64(time.Second))
		}
		if h.Title == "NA" {
			h.Title = ""
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
		return nil, &Error{Kind: KindFailed, Detail: fmt.Sprintf("search returned invalid video ids %q", bad)}
	}
	return nil, ErrNoResults
}
