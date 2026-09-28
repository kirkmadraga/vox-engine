package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bot/internal/ytdlp"
)

const vid = "jNQXAC9IVRw"

// fakeFetcher writes a file named <id>.opus into the given dir. If gate is
// set, it waits for it first (to hold a download "in progress").
type fakeFetcher struct {
	calls   atomic.Int32
	gate    chan struct{}
	err     error
	content string
	started chan string // receives the scratch dir when a fetch begins
}

func (f *fakeFetcher) Fetch(ctx context.Context, id, dir string) (ytdlp.Result, error) {
	f.calls.Add(1)
	if f.started != nil {
		f.started <- dir
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ytdlp.Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		os.WriteFile(filepath.Join(dir, id+".opus.part"), []byte("partial"), 0o644) // leave junk behind
		return ytdlp.Result{}, f.err
	}
	p := filepath.Join(dir, id+".opus")
	content := f.content
	if content == "" {
		content = "audio:" + id
	}
	return ytdlp.Result{Path: p, Title: "Title of " + id, Duration: 19 * time.Second}, os.WriteFile(p, []byte(content), 0o644)
}

func newCache(t *testing.T, f Fetcher, opts ...func(*Options)) (*Cache, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	o := Options{Fetcher: f}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := New(context.Background(), dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func TestMissThenHit(t *testing.T) {
	f := &fakeFetcher{}
	c, dir := newCache(t, f)
	ctx := context.Background()

	want := Entry{ID: vid, Path: filepath.Join(dir, vid+".opus"), Title: "Title of " + vid, Duration: 19 * time.Second}
	e, hit, err := c.Get(ctx, vid)
	if err != nil || hit || e != want {
		t.Fatalf("first Get = %+v, %v, %v; want %+v", e, hit, err, want)
	}
	if data, _ := os.ReadFile(e.Path); string(data) != "audio:"+vid {
		t.Errorf("content = %q", data)
	}
	// The hit reads title and duration back from the sidecar.
	e2, hit, err := c.Get(ctx, vid)
	if err != nil || !hit || e2 != want {
		t.Errorf("second Get = %+v, %v, %v; want %+v", e2, hit, err, want)
	}
	if f.calls.Load() != 1 {
		t.Errorf("fetches = %d, want 1", f.calls.Load())
	}
	assertOnlyAudio(t, dir, vid+".json", vid+".opus")
}

func TestHitWithoutSidecarHasNoTitle(t *testing.T) {
	c, dir := newCache(t, &fakeFetcher{})
	os.WriteFile(filepath.Join(dir, vid+".opus"), []byte("x"), 0o644) // e.g. cached before titles existed
	e, hit, err := c.Get(context.Background(), vid)
	if err != nil || !hit || e.Title != "" || e.Duration != 0 || e.Path != c.Path(vid) {
		t.Errorf("Get = %+v, %v, %v", e, hit, err)
	}
}

func TestConcurrentRequestsShareOneDownload(t *testing.T) {
	f := &fakeFetcher{gate: make(chan struct{})}
	c, _ := newCache(t, f)

	const n = 10
	var wg sync.WaitGroup
	entries := make([]Entry, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries[i], _, errs[i] = c.Get(context.Background(), vid)
		}()
	}
	time.Sleep(50 * time.Millisecond) // let all requests queue on the one download
	close(f.gate)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || entries[i].Path != c.Path(vid) || entries[i].Title == "" {
			t.Errorf("request %d: %+v, %v", i, entries[i], errs[i])
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}
}

func TestPartialDownloadNeverVisible(t *testing.T) {
	f := &fakeFetcher{gate: make(chan struct{}), started: make(chan string, 1)}
	c, dir := newCache(t, f)

	result := make(chan error, 1)
	go func() { _, _, err := c.Get(context.Background(), vid); result <- err }()
	scratch := <-f.started

	// Mid-download: the final name must not exist, and the scratch dir is hidden.
	if c.Has(vid) {
		t.Fatal("cache reports a hit during download")
	}
	if !strings.HasPrefix(filepath.Base(scratch), tmpPrefix) || filepath.Dir(scratch) != dir {
		t.Errorf("scratch dir %q should be a %s* dir inside the cache", scratch, tmpPrefix)
	}
	close(f.gate)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	assertOnlyAudio(t, dir, vid+".json", vid+".opus")
}

func TestFailedDownloadLeavesNothingAndRetries(t *testing.T) {
	boom := &ytdlp.Error{Kind: ytdlp.KindPrivate, Detail: "private"}
	f := &fakeFetcher{err: boom}
	c, dir := newCache(t, f)

	if _, _, err := c.Get(context.Background(), vid); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	assertOnlyAudio(t, dir) // no final file, no scratch dir, no .part junk

	f.err = nil // e.g. the video became public
	if _, hit, err := c.Get(context.Background(), vid); err != nil || hit {
		t.Errorf("retry: hit=%v err=%v (failures must not be cached)", hit, err)
	}
	if f.calls.Load() != 2 {
		t.Errorf("fetches = %d, want 2", f.calls.Load())
	}
}

func TestValidationRejectsBadFile(t *testing.T) {
	f := &fakeFetcher{content: "not really audio"}
	c, dir := newCache(t, f, func(o *Options) {
		o.Validate = func(path string) error {
			data, _ := os.ReadFile(path)
			if string(data) != "good" {
				return errors.New("bad frames")
			}
			return nil
		}
	})
	if _, _, err := c.Get(context.Background(), vid); err == nil || !strings.Contains(err.Error(), "bad frames") {
		t.Fatalf("err = %v", err)
	}
	assertOnlyAudio(t, dir)
}

func TestCallerCancelDoesNotAbortSharedDownload(t *testing.T) {
	f := &fakeFetcher{gate: make(chan struct{}), started: make(chan string, 1)}
	c, _ := newCache(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, _, err := c.Get(ctx, vid); first <- err }()
	<-f.started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: err = %v", err)
	}

	second := make(chan error, 1)
	go func() { _, _, err := c.Get(context.Background(), vid); second <- err }()
	time.Sleep(20 * time.Millisecond)
	close(f.gate)
	if err := <-second; err != nil {
		t.Fatalf("second caller: %v", err)
	}
	if f.calls.Load() != 1 || !c.Has(vid) {
		t.Errorf("fetches=%d has=%v: the download should have continued and been shared", f.calls.Load(), c.Has(vid))
	}
}

func TestShutdownCancelsDownloads(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	f := &fakeFetcher{gate: make(chan struct{}), started: make(chan string, 1)}
	c, err := New(base, filepath.Join(t.TempDir(), "cache"), Options{Fetcher: f})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, _, err := c.Get(context.Background(), vid); result <- err }()
	<-f.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestMaxDownloadsLimitsConcurrency(t *testing.T) {
	var running, peak atomic.Int32
	gate := make(chan struct{})
	f := fetcherFunc(func(ctx context.Context, id, dir string) (ytdlp.Result, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-gate
		running.Add(-1)
		p := filepath.Join(dir, id+".opus")
		return ytdlp.Result{Path: p}, os.WriteFile(p, []byte("x"), 0o644)
	})
	c, _ := newCache(t, f, func(o *Options) { o.MaxDownloads = 2 })

	ids := []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc", "ddddddddddd"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); c.Get(context.Background(), id) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Errorf("peak concurrent downloads = %d, want 2", p)
	}
	for _, id := range ids {
		if !c.Has(id) {
			t.Errorf("%s not cached", id)
		}
	}
}

func TestInvalidIDRejectedBeforeAnyIO(t *testing.T) {
	f := &fakeFetcher{}
	c, _ := newCache(t, f)
	for _, id := range []string{"../../../etc", "short", "has space!!"} {
		if _, _, err := c.Get(context.Background(), id); err == nil {
			t.Errorf("Get(%q) should fail", id)
		}
	}
	if f.calls.Load() != 0 {
		t.Error("fetcher must not run for invalid ids")
	}
}

func TestNewCleansStaleScratchDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	os.MkdirAll(filepath.Join(dir, tmpPrefix+vid+"-123"), 0o755)
	os.WriteFile(filepath.Join(dir, tmpPrefix+vid+"-123", "half.webm"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep1234567.opus"), []byte("x"), 0o644)
	if _, err := New(context.Background(), dir, Options{Fetcher: &fakeFetcher{}}); err != nil {
		t.Fatal(err)
	}
	assertOnlyAudio(t, dir, "keep1234567.opus")
}

func TestSize(t *testing.T) {
	c, dir := newCache(t, &fakeFetcher{})
	os.WriteFile(filepath.Join(dir, "aaaaaaaaaaa.opus"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(dir, "bbbbbbbbbbb.opus"), make([]byte, 50), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), make([]byte, 999), 0o644)
	os.MkdirAll(filepath.Join(dir, tmpPrefix+"x"), 0o755)
	os.WriteFile(filepath.Join(dir, tmpPrefix+"x", "big.opus"), make([]byte, 999), 0o644)
	if n, err := c.Size(); err != nil || n != 150 {
		t.Errorf("Size = %d, %v; want 150", n, err)
	}
}

type fetcherFunc func(ctx context.Context, id, dir string) (ytdlp.Result, error)

func (f fetcherFunc) Fetch(ctx context.Context, id, dir string) (ytdlp.Result, error) {
	return f(ctx, id, dir)
}

// assertOnlyAudio checks that dir contains exactly the named files and nothing else.
func assertOnlyAudio(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("cache dir contains %v, want %v", got, names)
	}
}
