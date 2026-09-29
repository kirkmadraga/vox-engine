package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSearchSuccess(t *testing.T) {
	r := &fakeRunner{stdout: "VOXMETA jNQXAC9IVRw 19 Me at the zoo\n"}
	s := &Searcher{Runner: r, Path: "yt-dlp", JSRuntime: "bun", Cookies: "data/cookies.txt"}
	hits, err := s.SearchN(context.Background(), "me at the zoo", 1)
	if err != nil || len(hits) != 1 || hits[0] != (Hit{ID: vid, Title: "Me at the zoo", Duration: 19 * time.Second}) {
		t.Fatalf("SearchN = %+v, %v", hits, err)
	}
	if r.gotName != "yt-dlp" {
		t.Errorf("ran %q", r.gotName)
	}
	for _, want := range [][]string{
		{"--flat-playlist"},
		{"--js-runtimes", "bun"},
		{"--cookies", "data/cookies.txt"},
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
	args := (&Searcher{}).SearchArgs("x", 1)
	for _, flag := range []string{"--js-runtimes", "--cookies"} {
		if slices.Contains(args, flag) {
			t.Errorf("%s must be omitted when not configured", flag)
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

func TestSearchNoResults(t *testing.T) {
	for _, q := range []string{"nothing matches", "   "} {
		s := &Searcher{Runner: &fakeRunner{stdout: ""}}
		if _, err := s.SearchN(context.Background(), q, 1); !errors.Is(err, ErrNoResults) {
			t.Errorf("SearchN(%q) err = %v, want ErrNoResults", q, err)
		}
	}
}

func TestSearchRejectsBadID(t *testing.T) {
	s := &Searcher{Runner: &fakeRunner{stdout: "VOXMETA ../../etc/passwd NA x\n"}}
	var yerr *Error
	if _, err := s.SearchN(context.Background(), "x", 1); !errors.As(err, &yerr) || yerr.Kind != KindFailed {
		t.Errorf("err = %v, want KindFailed", err)
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
	r := &fakeRunner{stdout: "noise\n" +
		"VOXMETA jNQXAC9IVRw 19.5 Me at the zoo\n" +
		"VOXMETA UCxxxxxxxxxxxxxxxxxxxxxx NA Some channel\n" + // not a video: skipped
		"VOXMETA dQw4w9WgXcQ NA NA\n" +
		"VOXMETA abcdefghijk 60 Third\n"}
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
	return []byte("VOXMETA jNQXAC9IVRw 19 x\n"), nil, nil
}

func TestSearchesRunOneAtATime(t *testing.T) {
	r := &countingRunner{}
	s := &Searcher{Runner: r}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SearchN(context.Background(), "q", 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if r.peak != 1 {
		t.Errorf("%d searches ran at once, want 1", r.peak)
	}
}
