package cache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

const vid = "jNQXAC9IVRw"

// memIndex is an in-package Index (cachetest.Memory can't be imported here:
// it imports this package).
type memIndex struct {
	mu   sync.Mutex
	recs map[string]Record
}

func newMemIndex() *memIndex { return &memIndex{recs: map[string]Record{}} }

func (m *memIndex) Put(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[r.ID] = r
	return nil
}
func (m *memIndex) Get(_ context.Context, id string) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	return r, ok, nil
}
func (m *memIndex) MarkPlayed(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.recs[id]; ok {
		r.LastPlayed, r.PlayCount = at, r.PlayCount+1
		m.recs[id] = r
	}
	return nil
}
func (m *memIndex) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, id)
	return nil
}
func (m *memIndex) All(context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, id := range slices.Sorted(maps.Keys(m.recs)) {
		out = append(out, m.recs[id])
	}
	return out, nil
}
func (m *memIndex) ids() []string {
	all, _ := m.All(context.Background())
	var out []string
	for _, r := range all {
		out = append(out, r.ID)
	}
	return out
}

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

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
func newClock() *clock               { return &clock{now: time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)} }

func newCache(t *testing.T, f Fetcher, opts ...func(*Options)) (*Cache, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := newCacheIn(t, dir, f, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func newCacheIn(t *testing.T, dir string, f Fetcher, opts ...func(*Options)) (*Cache, error) {
	t.Helper()
	o := Options{Fetcher: f, Index: newMemIndex()}
	for _, fn := range opts {
		fn(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c, err := New(ctx, dir, o)
	t.Cleanup(func() {
		cancel()
		if c != nil {
			c.Wait()
		}
	})
	return c, err
}

func TestMissThenHit(t *testing.T) {
	f := &fakeFetcher{}
	idx := newMemIndex()
	c, dir := newCache(t, f, func(o *Options) { o.Index = idx })
	ctx := context.Background()

	want := Entry{ID: vid, Path: filepath.Join(dir, vid+".opus"), Title: "Title of " + vid, Duration: 19 * time.Second}
	e, hit, err := c.Get(ctx, vid)
	if err != nil || hit || e != want {
		t.Fatalf("first Get = %+v, %v, %v; want %+v", e, hit, err, want)
	}
	if data, _ := os.ReadFile(e.Path); string(data) != "audio:"+vid {
		t.Errorf("content = %q", data)
	}
	// The hit reads title and duration back from the index.
	e2, hit, err := c.Get(ctx, vid)
	if err != nil || !hit || e2 != want {
		t.Errorf("second Get = %+v, %v, %v; want %+v", e2, hit, err, want)
	}
	if f.calls.Load() != 1 {
		t.Errorf("fetches = %d, want 1", f.calls.Load())
	}
	r, ok, _ := idx.Get(ctx, vid)
	if !ok || r.Size != int64(len("audio:"+vid)) || r.Title != "Title of "+vid || r.AddedAt.IsZero() {
		t.Errorf("index record = %+v, %v", r, ok)
	}
	assertFiles(t, dir, vid+".opus")
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
	assertFiles(t, dir, vid+".opus")
}

func TestFailedDownloadLeavesNothingAndRetries(t *testing.T) {
	boom := &ytdlp.Error{Kind: ytdlp.KindPrivate, Detail: "private"}
	f := &fakeFetcher{err: boom}
	idx := newMemIndex()
	c, dir := newCache(t, f, func(o *Options) { o.Index = idx })

	if _, _, err := c.Get(context.Background(), vid); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	assertFiles(t, dir) // no final file, no scratch dir, no .part junk
	if len(idx.ids()) != 0 {
		t.Errorf("failed download indexed: %v", idx.ids())
	}

	f.err = nil
	if _, hit, err := c.Get(context.Background(), vid); err != nil || hit {
		t.Errorf("retry: hit=%v err=%v (failures must not be cached)", hit, err)
	}
	if f.calls.Load() != 2 {
		t.Errorf("fetches = %d, want 2", f.calls.Load())
	}
}

func TestValidationRejectsBadFile(t *testing.T) {
	f := &fakeFetcher{content: "not really audio"}
	idx := newMemIndex()
	c, dir := newCache(t, f, func(o *Options) {
		o.Index = idx
		o.Validate = func(string) error { return errors.New("bad frames") }
	})
	if _, _, err := c.Get(context.Background(), vid); err == nil || !strings.Contains(err.Error(), "bad frames") {
		t.Fatalf("err = %v", err)
	}
	assertFiles(t, dir)
	if len(idx.ids()) != 0 {
		t.Error("rejected file must not be indexed")
	}
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
	c, err := New(base, filepath.Join(t.TempDir(), "cache"), Options{Fetcher: f, Index: newMemIndex()})
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

func TestStartupReconcile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	os.MkdirAll(filepath.Join(dir, tmpPrefix+vid+"-123"), 0o755)
	os.WriteFile(filepath.Join(dir, tmpPrefix+vid+"-123", "half.webm"), []byte("x"), 0o644)
	write := func(name string) { os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644) }
	write("known000000.opus") // indexed: kept
	write("stray000000.opus") // not indexed: deleted
	write("stray000000.json") // legacy title file: deleted
	write("tone.opus")        // not a video name: never touched
	write("notes.txt")        // not ours: never touched
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	idx := newMemIndex()
	idx.Put(context.Background(), Record{ID: "known000000", Size: 1})
	idx.Put(context.Background(), Record{ID: "gone0000000", Size: 1}) // file missing: dropped

	if _, err := newCacheIn(t, dir, &fakeFetcher{}, func(o *Options) { o.Index = idx }); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, "known000000.opus", "notes.txt", "subdir", "tone.opus")
	if got := idx.ids(); !slices.Equal(got, []string{"known000000"}) {
		t.Errorf("index after reconcile = %v", got)
	}
}

func TestPurgeByAgeUsesLastPlayed(t *testing.T) {
	clk := newClock()
	idx := newMemIndex()
	c, dir := newCache(t, &fakeFetcher{}, func(o *Options) {
		o.Index, o.Now, o.MaxAge = idx, clk.Now, 12*time.Hour
	})
	ctx := context.Background()
	c.Get(ctx, "aaaaaaaaaaa")
	c.Get(ctx, "bbbbbbbbbbb")

	clk.add(10 * time.Hour)
	c.MarkPlayed("bbbbbbbbbbb") // played 10 h after download
	clk.add(3 * time.Hour)      // a: 13 h since download; b: 3 h since play
	c.Purge(ctx)

	assertFiles(t, dir, "bbbbbbbbbbb.opus")
	if got := idx.ids(); !slices.Equal(got, []string{"bbbbbbbbbbb"}) {
		t.Errorf("index = %v", got)
	}
}

func TestPurgeBySizeEvictsLeastRecentlyUsed(t *testing.T) {
	clk := newClock()
	f := &fakeFetcher{content: strings.Repeat("x", 100)}
	c, dir := newCache(t, f, func(o *Options) { o.Now, o.MaxBytes = clk.Now, 250 })
	ctx := context.Background()
	for _, id := range []string{"aaaaaaaaaaa", "bbbbbbbbbbb"} {
		c.Get(ctx, id)
		clk.add(time.Minute)
	}
	c.MarkPlayed("aaaaaaaaaaa") // a is now the most recently used
	clk.add(time.Minute)
	c.Get(ctx, "ccccccccccc") // 300 bytes > 250: evict the LRU, which is b

	waitFor(t, func() bool { return !c.Has("bbbbbbbbbbb") })
	assertFiles(t, dir, "aaaaaaaaaaa.opus", "ccccccccccc.opus")
	if size, _ := c.Size(ctx); size != 200 {
		t.Errorf("size = %d, want 200", size)
	}
}

func TestPurgeNeverTouchesInUseOrJustDownloaded(t *testing.T) {
	clk := newClock()
	inUse := map[string]bool{}
	var mu sync.Mutex
	f := &fakeFetcher{content: strings.Repeat("x", 100)}
	c, dir := newCache(t, f, func(o *Options) {
		o.Now, o.MaxBytes, o.MaxAge = clk.Now, 150, time.Hour
		o.InUse = func() map[string]bool { mu.Lock(); defer mu.Unlock(); return maps.Clone(inUse) }
	})
	ctx := context.Background()
	mu.Lock()
	inUse["aaaaaaaaaaa"] = true // e.g. playing right now
	mu.Unlock()
	c.Get(ctx, "aaaaaaaaaaa")
	clk.add(2 * time.Hour) // a is past MaxAge, but in use
	c.Get(ctx, "bbbbbbbbbbb")
	// 200 bytes > 150, a is in use and b was just downloaded: nothing may go.
	c.Purge(ctx, "bbbbbbbbbbb")
	assertFiles(t, dir, "aaaaaaaaaaa.opus", "bbbbbbbbbbb.opus")

	mu.Lock()
	delete(inUse, "aaaaaaaaaaa") // finished playing
	mu.Unlock()
	c.Purge(ctx)
	assertFiles(t, dir, "bbbbbbbbbbb.opus")
}

func TestPurgeLoopRunsInBackground(t *testing.T) {
	clk := newClock()
	c, dir := newCache(t, &fakeFetcher{}, func(o *Options) {
		o.Now, o.MaxAge, o.PurgeInterval = clk.Now, time.Hour, 10*time.Millisecond
	})
	c.Get(context.Background(), "aaaaaaaaaaa")
	clk.add(2 * time.Hour)
	waitFor(t, func() bool { return !c.Has("aaaaaaaaaaa") })
	assertFiles(t, dir)
}

func TestNewRequiresIndex(t *testing.T) {
	if _, err := New(context.Background(), t.TempDir(), Options{Fetcher: &fakeFetcher{}}); err == nil {
		t.Error("New without an Index should fail")
	}
}

type fetcherFunc func(ctx context.Context, id, dir string) (ytdlp.Result, error)

func (f fetcherFunc) Fetch(ctx context.Context, id, dir string) (ytdlp.Result, error) {
	return f(ctx, id, dir)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertFiles checks that dir contains exactly the named entries and nothing else.
func assertFiles(t *testing.T, dir string, names ...string) {
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

func TestDownloadsStartAtLeastMinIntervalApart(t *testing.T) {
	clk := newClock()
	var mu sync.Mutex
	var starts []time.Time
	var slept []time.Duration
	f := fetcherFunc(func(ctx context.Context, id, dir string) (ytdlp.Result, error) {
		mu.Lock()
		starts = append(starts, clk.Now())
		mu.Unlock()
		clk.add(5 * time.Second) // the download itself takes 5 s
		p := filepath.Join(dir, id+".opus")
		return ytdlp.Result{Path: p}, os.WriteFile(p, []byte("x"), 0o644)
	})
	c, _ := newCache(t, f, func(o *Options) {
		o.Now, o.MinInterval = clk.Now, 30*time.Second
		o.Sleep = func(_ context.Context, d time.Duration) error {
			mu.Lock()
			slept = append(slept, d)
			mu.Unlock()
			clk.add(d)
			return nil
		}
	})

	ctx := context.Background()
	for _, id := range []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"} {
		if _, _, err := c.Get(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if len(starts) != 3 {
		t.Fatalf("starts = %v", starts)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 30*time.Second {
			t.Errorf("download %d started %v after the previous one, want >= 30s", i, gap)
		}
	}
	// The first download doesn't wait; later ones wait only the remainder (30 s - 5 s of download).
	if len(slept) != 2 || slept[0] != 25*time.Second {
		t.Errorf("slept = %v, want [25s 25s]", slept)
	}

	// Cache hits never wait or touch the gate.
	before := len(slept)
	c.Get(ctx, "aaaaaaaaaaa")
	if len(slept) != before {
		t.Error("a cache hit must not wait")
	}
}

func TestMinIntervalAlreadyElapsedDoesNotWait(t *testing.T) {
	clk := newClock()
	waited := false
	c, _ := newCache(t, &fakeFetcher{}, func(o *Options) {
		o.Now, o.MinInterval = clk.Now, 30*time.Second
		o.Sleep = func(context.Context, time.Duration) error { waited = true; return nil }
	})
	c.Get(context.Background(), "aaaaaaaaaaa")
	clk.add(time.Minute)
	c.Get(context.Background(), "bbbbbbbbbbb")
	if waited {
		t.Error("no wait expected when the interval has already passed")
	}
}

func TestShutdownWhileWaitingForTurn(t *testing.T) {
	clk := newClock()
	base, cancel := context.WithCancel(context.Background())
	c, err := New(base, filepath.Join(t.TempDir(), "cache"), Options{
		Fetcher: &fakeFetcher{}, Index: newMemIndex(), Now: clk.Now, MinInterval: time.Hour,
		Sleep: func(ctx context.Context, d time.Duration) error { <-ctx.Done(); return ctx.Err() },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Get(context.Background(), "aaaaaaaaaaa")
	result := make(chan error, 1)
	go func() { _, _, err := c.Get(context.Background(), "bbbbbbbbbbb"); result <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if c.Has("bbbbbbbbbbb") {
		t.Error("download ran despite shutdown")
	}
}

// cache_max_bytes: 0 and cache_max_age: 0 each mean "no limit": nothing is
// deleted for that reason, however big or old the cache gets, also while the
// other limit is on.
func TestZeroLimitsNeverPurge(t *testing.T) {
	cases := map[string]struct {
		maxBytes int64
		maxAge   time.Duration
	}{
		"both off":                   {0, 0},
		"age off, generous size cap": {1 << 20, 0},
		"size off, age limit on":     {0, 12 * time.Hour},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			clk := newClock()
			f := &fakeFetcher{content: strings.Repeat("x", 1000)}
			cache, dir := newCache(t, f, func(o *Options) { o.Now, o.MaxBytes, o.MaxAge = clk.Now, c.maxBytes, c.maxAge })
			ctx := context.Background()
			for _, id := range []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"} {
				if _, _, err := cache.Get(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			if c.maxAge == 0 {
				clk.add(24 * 365 * time.Hour) // a year unplayed
			} else {
				clk.add(time.Hour) // well within the age limit
			}
			cache.Purge(ctx)
			assertFiles(t, dir, "aaaaaaaaaaa.opus", "bbbbbbbbbbb.opus", "ccccccccccc.opus")
		})
	}
}

// The "stored" log line says how long checking the file took.
func TestStoredLogsValidateTime(t *testing.T) {
	var buf bytes.Buffer
	clk := newClock()
	c, _ := newCache(t, &fakeFetcher{}, func(o *Options) {
		o.Now = clk.Now
		o.Logger = slog.New(slog.NewTextHandler(&buf, nil))
		o.Validate = func(string) error { clk.add(1500 * time.Millisecond); return nil }
	})
	if _, _, err := c.Get(context.Background(), "aaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `msg="cache: stored"`) || !strings.Contains(buf.String(), "validate=1.5s") {
		t.Errorf("log: %s", buf.String())
	}
}
