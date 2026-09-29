package ytdlp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// result is one line of yt-dlp search output, as printed via searchFields.
type result struct {
	ID       string  `json:"id"`
	Duration float64 `json:"duration,omitempty"`
	Title    string  `json:"title,omitempty"`
	Channel  string  `json:"channel,omitempty"`
	Verified bool    `json:"channel_is_verified,omitempty"`
}

// out renders search output lines.
func out(rs ...result) string {
	var b strings.Builder
	for _, r := range rs {
		j, _ := json.Marshal(r)
		b.WriteString(metaMarker + string(j) + "\n")
	}
	return b.String()
}

var zooLine = out(result{ID: vid, Duration: 19, Title: "x"})

func TestSearchSuccess(t *testing.T) {
	r := &fakeRunner{stdout: out(result{ID: vid, Duration: 19, Title: "Me at the zoo", Channel: "jawed", Verified: true})}
	s := &Searcher{Runner: r, Path: "yt-dlp", JSRuntime: "bun"} // cookies: see cookies_test.go
	hits, err := s.SearchN(context.Background(), "me at the zoo", 1)
	want := Hit{ID: vid, Title: "Me at the zoo", Duration: 19 * time.Second, Channel: "jawed", Verified: true}
	if err != nil || len(hits) != 1 || hits[0] != want {
		t.Fatalf("SearchN = %+v, %v", hits, err)
	}
	if r.gotName != "yt-dlp" {
		t.Errorf("ran %q", r.gotName)
	}
	for _, want := range [][]string{
		{"--flat-playlist"},
		{"--js-runtimes", "bun"},
		{"--print", metaMarker + searchFields},
	} {
		if !containsSeq(r.gotArgs, want) {
			t.Errorf("args missing %v: %v", want, r.gotArgs)
		}
	}
	if last := r.gotArgs[len(r.gotArgs)-1]; last != "ytsearch1:me at the zoo" {
		t.Errorf("last arg = %q", last)
	}
}

func TestSearchArgsOmitUnsetFlags(t *testing.T) {
	for _, args := range [][]string{(&Searcher{}).SearchArgs("x", 1), (&Searcher{}).MusicSearchArgs("x", 1)} {
		for _, flag := range []string{"--js-runtimes", "--cookies"} {
			if slices.Contains(args, flag) {
				t.Errorf("%s must be omitted when not configured: %q", flag, args)
			}
		}
	}
}

func TestSearchQueryIsNeverAnOption(t *testing.T) {
	long := strings.Repeat("é", MaxQueryLen+50)
	cases := map[string]string{
		"--exec rm -rf /":  "ytsearch1:--exec rm -rf /",
		"a\nb\t c":         "ytsearch1:a b c",
		"  padded  words ": "ytsearch1:padded words",
		long:               "ytsearch1:" + strings.Repeat("é", MaxQueryLen),
	}
	for q, want := range cases {
		args := (&Searcher{}).SearchArgs(q, 1)
		if got := args[len(args)-1]; got != want {
			t.Errorf("SearchArgs(%q) last = %q, want %q", q, got, want)
		}
		// Everything but the last argument is fixed, whatever the query.
		if base := (&Searcher{}).SearchArgs("x", 1); !slices.Equal(args[:len(args)-1], base[:len(base)-1]) {
			t.Errorf("query %q changed other args: %q", q, args)
		}
	}
}

func TestMusicSearchURLEncodesQuery(t *testing.T) {
	for _, q := range []string{"AC/DC Back In Black", "Simon & Garfunkel", "#1 Crush", "a?b=c", "--exec rm", "  spaced\n out  "} {
		u, err := url.Parse(MusicSearchURL(q))
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if u.Host != "music.youtube.com" || u.Path != "/search" || u.Fragment != "songs" || len(u.Query()) != 1 {
			t.Errorf("%q: URL %q changed shape", q, u)
		}
		if got := u.Query().Get("q"); got != cleanQuery(q) {
			t.Errorf("%q: q = %q", q, got)
		}
	}
}

func TestSearchMusic(t *testing.T) {
	r := &fakeRunner{stdout: out(result{ID: "tYvFa2ARD24", Title: "Uptown Funk (feat. Bruno Mars)"}, result{ID: "RTrYmWOAdjo", Title: "Uptown Funk!"})}
	hits, err := (&Searcher{Runner: r}).SearchMusic(context.Background(), "Mark Ronson, Bruno Mars Uptown Funk", 3)
	want := []Hit{{ID: "tYvFa2ARD24", Title: "Uptown Funk (feat. Bruno Mars)"}, {ID: "RTrYmWOAdjo", Title: "Uptown Funk!"}}
	if err != nil || !slices.Equal(hits, want) {
		t.Fatalf("SearchMusic = %+v, %v", hits, err)
	}
	n := len(r.gotArgs)
	if !slices.Equal(r.gotArgs[n-3:], []string{"--playlist-items", "1:3", MusicSearchURL("Mark Ronson, Bruno Mars Uptown Funk")}) {
		t.Errorf("args = %q", r.gotArgs)
	}
}

func TestSearchNoResults(t *testing.T) {
	for _, q := range []string{"nothing matches", "   "} {
		s := &Searcher{Runner: &fakeRunner{stdout: ""}}
		if _, err := s.SearchN(context.Background(), q, 1); !errors.Is(err, ErrNoResults) {
			t.Errorf("SearchN(%q) err = %v, want ErrNoResults", q, err)
		}
		if _, err := s.SearchMusic(context.Background(), q, 1); !errors.Is(err, ErrNoResults) {
			t.Errorf("SearchMusic(%q) err = %v, want ErrNoResults", q, err)
		}
	}
}

func TestSearchRejectsBadOutput(t *testing.T) {
	for _, stdout := range []string{out(result{ID: "../../etc/passwd"}), metaMarker + "not json\n"} {
		s := &Searcher{Runner: &fakeRunner{stdout: stdout}}
		var yerr *Error
		if _, err := s.SearchN(context.Background(), "x", 1); !errors.As(err, &yerr) || yerr.Kind != KindFailed {
			t.Errorf("%q: err = %v, want KindFailed", stdout, err)
		}
	}
}

func TestSearchErrors(t *testing.T) {
	cases := []struct {
		name string
		r    *fakeRunner
		want Kind
	}{
		{"bot check", &fakeRunner{err: exitErr, stderr: "ERROR: Sign in to confirm you're not a bot"}, KindBotCheck},
		{"missing tool", &fakeRunner{err: fmt.Errorf("exec: %w", exec.ErrNotFound)}, KindMissingTool},
		{"other", &fakeRunner{err: exitErr, stderr: "ERROR: something broke"}, KindFailed},
		{"timeout", &fakeRunner{block: true}, KindTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Searcher{Runner: c.r, Timeout: 10 * time.Millisecond}
			var yerr *Error
			if _, err := s.SearchN(context.Background(), "x", 1); !errors.As(err, &yerr) || yerr.Kind != c.want {
				t.Errorf("err = %v, want kind %d", err, c.want)
			}
		})
	}
}

func TestSearchNParsesResults(t *testing.T) {
	r := &fakeRunner{stdout: "noise\n" + out(
		result{ID: vid, Duration: 19.5, Title: "Me at the zoo"},
		result{ID: "UCxxxxxxxxxxxxxxxxxxxxxx", Title: "Some channel"}, // not a video: skipped
		result{ID: "dQw4w9WgXcQ"},
		result{ID: "abcdefghijk", Duration: 60, Title: "Third"},
	)}
	hits, err := (&Searcher{Runner: r}).SearchN(context.Background(), "q", 2)
	want := []Hit{
		{ID: vid, Title: "Me at the zoo", Duration: 19500 * time.Millisecond},
		{ID: "dQw4w9WgXcQ"}, // unknown duration and title
	}
	if err != nil || !slices.Equal(hits, want) {
		t.Fatalf("SearchN = %+v, %v; want %+v", hits, err, want)
	}
	if last := r.gotArgs[len(r.gotArgs)-1]; last != "ytsearch2:q" {
		t.Errorf("last arg = %q", last)
	}
}

// yt-dlp prints null for missing fields.
func TestSearchParsesNulls(t *testing.T) {
	stdout := metaMarker + `{"id": "dQw4w9WgXcQ", "duration": null, "title": "T", "channel": null, "channel_is_verified": null}` + "\n"
	hits, err := (&Searcher{Runner: &fakeRunner{stdout: stdout}}).SearchN(context.Background(), "q", 1)
	if err != nil || len(hits) != 1 || hits[0] != (Hit{ID: "dQw4w9WgXcQ", Title: "T"}) {
		t.Errorf("hits = %+v, %v", hits, err)
	}
}

func TestSearchNClampsCount(t *testing.T) {
	for n, want := range map[int]string{0: "ytsearch1:q", -3: "ytsearch1:q", 99: fmt.Sprintf("ytsearch%d:q", MaxResults)} {
		r := &fakeRunner{}
		(&Searcher{Runner: r}).SearchN(context.Background(), "q", n)
		if last := r.gotArgs[len(r.gotArgs)-1]; last != want {
			t.Errorf("n=%d: last arg = %q, want %q", n, last, want)
		}
	}
}

// countingRunner tracks how many runs overlap.
type countingRunner struct {
	mu            sync.Mutex
	running, peak int
}

func (c *countingRunner) Run(context.Context, string, []string) ([]byte, []byte, error) {
	c.mu.Lock()
	c.running++
	c.peak = max(c.peak, c.running)
	c.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	c.mu.Lock()
	c.running--
	c.mu.Unlock()
	return []byte(zooLine), nil, nil
}

func TestSearchesRunOneAtATime(t *testing.T) {
	r := &countingRunner{}
	s := &Searcher{Runner: r}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			search := s.SearchN
			if i%2 == 1 {
				search = s.SearchMusic
			}
			if _, err := search(context.Background(), "q", 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if r.peak != 1 {
		t.Errorf("%d searches ran at once, want 1", r.peak)
	}
}
