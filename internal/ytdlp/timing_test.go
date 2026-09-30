package ytdlp

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newFakeClock() *fakeClock           { return &fakeClock{t: time.Unix(1000, 0)} }

// step is one stdout line, printed after some time passes.
type step struct {
	after time.Duration
	line  string
}

// lineRunner plays a script of stdout lines against a fake clock, like yt-dlp
// printing its stage markers as it goes.
type lineRunner struct {
	clock  *fakeClock
	script []step
	tail   time.Duration // time after the last line until exit
	err    error
	stderr string
}

func (r *lineRunner) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	return r.RunLines(ctx, name, args, nil)
}

func (r *lineRunner) RunLines(_ context.Context, _ string, _ []string, onLine func(string)) ([]byte, []byte, error) {
	var out strings.Builder
	for _, s := range r.script {
		r.clock.add(s.after)
		out.WriteString(s.line + "\n")
		if onLine != nil {
			onLine(s.line)
		}
	}
	r.clock.add(r.tail)
	return []byte(out.String()), []byte(r.stderr), r.err
}

func logger() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

// A normal download: extraction (the slow JS challenge), transfer, conversion.
var fullScript = []step{
	{41800 * time.Millisecond, "VOXMETA False 19.5 Me at the zoo"},
	{50 * time.Millisecond, "VOXSTAGE download"},
	{3200 * time.Millisecond, "VOXSTAGE convert"},
	{400 * time.Millisecond, "VOXFILE /tmp/dl/jNQXAC9IVRw.opus"},
}

func TestDownloadTimingPhases(t *testing.T) {
	clock := newFakeClock()
	log, buf := logger()
	d := Downloader{Runner: &lineRunner{clock: clock, script: fullScript, tail: 100 * time.Millisecond}, Logger: log, Now: clock.Now}
	res, err := d.Fetch(context.Background(), vid, "/tmp/dl")
	if err != nil || res.Path != "/tmp/dl/jNQXAC9IVRw.opus" || res.Title != "Me at the zoo" {
		t.Fatalf("Fetch = %+v, %v (downloads must work as before)", res, err)
	}
	want := "video=jNQXAC9IVRw extract=41.8s download=3.2s convert=400ms total=45.6s outcome=ok"
	if !strings.Contains(buf.String(), `msg="ytdlp: download timing" `+want) {
		t.Errorf("log:\n%s\nwant %q", buf.String(), want)
	}
}

// A runner without line output (e.g. a test fake) still gets its total logged.
func TestDownloadTimingWithoutLines(t *testing.T) {
	log, buf := logger()
	d := Downloader{Runner: &fakeRunner{stdout: "VOXMETA False 19 x\nVOXFILE /tmp/dl/x.opus\n"}, Logger: log}
	if _, err := d.Fetch(context.Background(), vid, "/tmp/dl"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "total=") || strings.Contains(got, "extract=") || !strings.Contains(got, "outcome=ok") {
		t.Errorf("log: %s", got)
	}
}

func TestDownloadTimingOnFailure(t *testing.T) {
	clock := newFakeClock()
	log, buf := logger()
	r := &lineRunner{clock: clock, tail: 2 * time.Second, err: exitErr, stderr: "ERROR: Sign in to confirm you're not a bot"}
	d := Downloader{Runner: r, Logger: log, Now: clock.Now}
	if _, err := d.Fetch(context.Background(), vid, "/tmp/dl"); err == nil {
		t.Fatal("want an error")
	}
	if got := buf.String(); !strings.Contains(got, "total=2s outcome=bot-check") || strings.Contains(got, "extract=") {
		t.Errorf("log: %s", got)
	}
}

// Stages whose markers are missing are left out, never guessed.
func TestDownloadTimingPartialStages(t *testing.T) {
	clock := newFakeClock()
	log, buf := logger()
	script := []step{{5 * time.Second, "VOXMETA False 7200 Long"}} // skipped by --match-filter: no download
	d := Downloader{Runner: &lineRunner{clock: clock, script: script}, Logger: log, Now: clock.Now, MaxDuration: time.Hour}
	d.Fetch(context.Background(), vid, "/tmp/dl")
	got := buf.String()
	if !strings.Contains(got, "extract=5s total=5s outcome=too-long") || strings.Contains(got, "download=") {
		t.Errorf("log: %s", got)
	}
}

func TestNoLoggerNoTiming(t *testing.T) {
	d := Downloader{Runner: &fakeRunner{stdout: "VOXFILE /tmp/dl/x.opus\n"}}
	if _, err := d.Fetch(context.Background(), vid, "/tmp/dl"); err != nil {
		t.Fatal(err) // and no panic without a logger
	}
}

func TestArgsPrintStageMarkers(t *testing.T) {
	args := Downloader{}.Args(vid, "d")
	for _, want := range [][]string{
		{"--print", "pre_process:VOXMETA %(is_live)s %(duration)s %(title)s"},
		{"--print", "before_dl:VOXSTAGE download"},
		{"--print", "post_process:VOXSTAGE convert"},
		{"--print", "after_move:VOXFILE %(filepath)s"},
	} {
		if !containsSeq(args, want) {
			t.Errorf("args missing %q: %q", want, args)
		}
	}
}

func TestSearchTiming(t *testing.T) {
	clock := newFakeClock()
	log, buf := logger()
	s := &Searcher{Runner: &lineRunner{clock: clock, script: []step{{2400 * time.Millisecond, strings.TrimSpace(zooLine)}}}, Logger: log, Now: clock.Now}
	if _, err := s.SearchN(context.Background(), "me at the zoo", 10); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, `msg="ytdlp: search timing" kind=youtube results=1 took=2.4s waited=0s outcome=ok`) {
		t.Errorf("log: %s", got)
	}

	buf.Reset()
	s.Runner = &lineRunner{clock: clock, tail: time.Second}
	s.SearchMusic(context.Background(), "nothing", 3)
	if got := buf.String(); !strings.Contains(got, "kind=youtube-music results=0 took=1s waited=0s outcome=no-results") {
		t.Errorf("log: %s", got)
	}
}

// Time spent waiting for a free search slot is logged separately.
func TestSearchTimingWaited(t *testing.T) {
	clock := newFakeClock()
	log, buf := logger()
	s := &Searcher{Runner: &lineRunner{clock: clock, script: []step{{time.Second, strings.TrimSpace(zooLine)}}}, Logger: log, Now: clock.Now}
	release := s.acquire() // the only slot is busy
	done := make(chan struct{})
	go func() {
		s.SearchN(context.Background(), "q", 1)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond) // let it start waiting
	clock.add(3 * time.Second)
	release()
	<-done
	if got := buf.String(); !strings.Contains(got, "took=1s waited=3s") {
		t.Errorf("log: %s", got)
	}
}

func TestLineWriter(t *testing.T) {
	var lines []string
	w := &lineWriter{onLine: func(l string) { lines = append(lines, l) }}
	for _, chunk := range []string{"VOXME", "TA a\r\nVOXSTAGE down", "load\n", "tail without newline"} {
		w.Write([]byte(chunk))
	}
	if strings.Join(lines, "|") != "VOXMETA a|VOXSTAGE download" {
		t.Errorf("lines = %q", lines)
	}
	if w.all.String() != "VOXMETA a\r\nVOXSTAGE download\ntail without newline" {
		t.Errorf("all = %q (everything must be kept)", w.all.String())
	}
}

func TestKindNames(t *testing.T) {
	for k, want := range map[Kind]string{KindFailed: "failed", KindBotCheck: "bot-check", KindMissingTool: "missing-tool", Kind(99): "unknown"} {
		if k.String() != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), k.String(), want)
		}
	}
}

// The owner's debug command sees the last download and search, and searches
// running or waiting.
func TestRecentRunsAndSearchStatus(t *testing.T) {
	clock := newFakeClock()
	recent := &Recent{}
	d := Downloader{Runner: &lineRunner{clock: clock, script: fullScript, tail: 100 * time.Millisecond}, Now: clock.Now, Recent: recent}
	d.Fetch(context.Background(), vid, "/tmp/dl")
	dl, se := recent.Last()
	if dl.Took != 45600*time.Millisecond || dl.Extract != 41800*time.Millisecond || dl.Outcome != "ok" || dl.At.IsZero() || !se.At.IsZero() {
		t.Errorf("after a download: %+v / %+v", dl, se)
	}

	s := &Searcher{Runner: &lineRunner{clock: clock, script: []step{{time.Second, strings.TrimSpace(zooLine)}}}, Now: clock.Now, Recent: recent, MaxConcurrent: 1}
	if st := s.Status(); st != (SearchStatus{Max: 1}) {
		t.Errorf("idle: %+v", st)
	}
	release := s.acquire() // the only slot is busy
	done := make(chan struct{})
	go func() { s.SearchN(context.Background(), "q", 1); close(done) }()
	for range 200 {
		if s.Status().Waiting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if st := s.Status(); st.Running != 1 || st.Waiting != 1 {
		t.Errorf("busy: %+v", st)
	}
	release()
	<-done
	if _, se := recent.Last(); se.Took != time.Second || se.Outcome != "ok" {
		t.Errorf("last search: %+v", se)
	}
	var none *Recent // nil-safe when not configured
	none.setSearch(Run{})
	if a, b := none.Last(); !a.At.IsZero() || !b.At.IsZero() {
		t.Error("nil Recent must be empty")
	}
}
