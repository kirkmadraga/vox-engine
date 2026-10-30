package mediafile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var tone = filepath.Join("..", "..", "assets", "tone.opus")

// fakeFFmpeg "converts" by copying the test tone to the output path (the
// last argument), or fails as told.
type fakeFFmpeg struct {
	err    error
	stderr string
	args   []string
	input  []byte // what the input file held when ffmpeg ran
}

func (f *fakeFFmpeg) Run(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	f.args = args
	f.input, _ = os.ReadFile(args[slices.Index(args, "-i")+1])
	if f.err != nil {
		return nil, []byte(f.stderr), f.err
	}
	data, err := os.ReadFile(tone)
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, os.WriteFile(args[len(args)-1], data, 0o644)
}

func newFetcher(t *testing.T, ff Runner, mod func(*Options)) *Fetcher {
	t.Helper()
	opts := Options{Dir: tempDir(t), FFmpeg: "ffmpeg", Runner: ff, Client: http.DefaultClient, MaxBytes: 1000}
	if mod != nil {
		mod(&opts)
	}
	f, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// tempDir is t.TempDir with a patient cleanup: on Windows a file that was
// just removed (or is being scanned by antivirus) can keep its folder
// "not empty" for a moment.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mediafile-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; ; i++ {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if i == 20 {
				t.Errorf("remove %s: %v", dir, err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

// serve answers every request with body and the given content type.
func serve(t *testing.T, contentType, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func kindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return -1
}

func TestFetchDownloadsConvertsAndMeasures(t *testing.T) {
	ff := &fakeFFmpeg{}
	f := newFetcher(t, ff, nil)
	key := NewKey()
	res, err := f.Fetch(context.Background(), key, Source{URL: serve(t, "audio/mpeg", "ID3 fake mp3") + "/music/My%20Song.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "My Song.mp3" || res.Path != filepath.Join(f.opts.Dir, key+".opus") {
		t.Errorf("result = %+v", res)
	}
	if string(ff.input) != "ID3 fake mp3" {
		t.Errorf("ffmpeg read %q", ff.input)
	}
	if res.Duration < time.Second {
		t.Errorf("duration = %v, want the tone's length", res.Duration)
	}
	// Only the converted file is left.
	entries, _ := os.ReadDir(f.opts.Dir)
	if len(entries) != 1 || entries[0].Name() != key+".opus" {
		t.Errorf("dir holds %v", entries)
	}
}

func TestFetchErrors(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	big := strings.Repeat("x", 1001)
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		for range 11 {
			fmt.Fprint(w, strings.Repeat("x", 100))
			w.(http.Flusher).Flush() // no Content-Length: the cap must hold while reading
		}
	}))
	t.Cleanup(chunked.Close)
	// Says it's huge, then sends nothing: refused on the header alone.
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "1000000000")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(declared.Close)

	for name, c := range map[string]struct {
		src  Source
		ff   *fakeFFmpeg
		want Kind
	}{
		"web page":           {Source{URL: serve(t, "text/html; charset=utf-8", "<html>")}, nil, KindPage},
		"xhtml page":         {Source{URL: serve(t, "application/xhtml+xml", "<html>")}, nil, KindPage},
		"zip":                {Source{URL: serve(t, "application/zip", "PK")}, nil, KindNotMedia},
		"json":               {Source{URL: serve(t, "application/json", "{}")}, nil, KindNotMedia},
		"plain text":         {Source{URL: serve(t, "text/plain", "hi")}, nil, KindNotMedia},
		"http error":         {Source{URL: notFound.URL}, nil, KindHTTP},
		"unreachable":        {Source{URL: "http://127.0.0.1:1/x.mp3"}, nil, KindUnreachable},
		"not http":           {Source{URL: "file:///etc/passwd"}, nil, KindBadLink},
		"too big (length)":   {Source{URL: serve(t, "audio/mpeg", big)}, nil, KindTooBig},
		"too big (chunked)":  {Source{URL: chunked.URL}, nil, KindTooBig},
		"too big (header)":   {Source{URL: declared.URL}, nil, KindTooBig},
		"too big (declared)": {Source{URL: "http://127.0.0.1:1/x.mp3", Size: 1001}, nil, KindTooBig},
		"empty":              {Source{URL: serve(t, "audio/mpeg", "")}, nil, KindNoAudio},
		"ffmpeg fails":       {Source{URL: serve(t, "application/octet-stream", "junk")}, &fakeFFmpeg{err: errors.New("exit status 1"), stderr: "Invalid data"}, KindNoAudio},
		"ffmpeg missing":     {Source{URL: serve(t, "application/octet-stream", "junk")}, &fakeFFmpeg{err: exec.ErrNotFound}, KindMissingTool},
	} {
		t.Run(name, func(t *testing.T) {
			ff := c.ff
			if ff == nil {
				ff = &fakeFFmpeg{}
			}
			f := newFetcher(t, ff, nil)
			_, err := f.Fetch(context.Background(), NewKey(), c.src)
			if got := kindOf(err); got != c.want {
				t.Fatalf("err = %v (%v), want %v", err, got, c.want)
			}
			if entries, _ := os.ReadDir(f.opts.Dir); len(entries) != 0 {
				t.Errorf("left behind %v", entries)
			}
		})
	}
}

func TestFetchRefusesTooLong(t *testing.T) {
	f := newFetcher(t, &fakeFFmpeg{}, func(o *Options) { o.MaxDuration = 500 * time.Millisecond })
	_, err := f.Fetch(context.Background(), NewKey(), Source{URL: serve(t, "audio/ogg", "x")})
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindTooLong || e.MaxLen != 500*time.Millisecond {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchRefusesUnusableConversion(t *testing.T) {
	ff := RunnerFunc(func(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
		return nil, nil, os.WriteFile(args[len(args)-1], []byte("not ogg"), 0o644)
	})
	f := newFetcher(t, ff, nil)
	if _, err := f.Fetch(context.Background(), NewKey(), Source{URL: serve(t, "application/octet-stream", "x")}); kindOf(err) != KindNoAudio {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		fmt.Fprint(w, "x")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	f := newFetcher(t, &fakeFFmpeg{}, func(o *Options) { o.DownloadTimeout = 100 * time.Millisecond })
	if _, err := f.Fetch(context.Background(), NewKey(), Source{URL: slow.URL}); kindOf(err) != KindTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchRejectsBadKeys(t *testing.T) {
	f := newFetcher(t, &fakeFFmpeg{}, nil)
	for _, k := range []string{"", "file-", "file-../x", "abc", "file-ABC", "dQw4w9WgXcQ"} {
		if _, err := f.Fetch(context.Background(), k, Source{URL: "http://x/"}); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	if k := NewKey(); !validKey(k) {
		t.Errorf("NewKey() = %q is invalid", k)
	}
}

func TestConvertArgs(t *testing.T) {
	args := ConvertArgs("in", "out", 90*time.Second)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-protocol_whitelist file", "-i in", "-map 0:a:0", "-c:a libopus", "-frame_duration 20",
		"-ar 48000", "-t 91", "-f ogg -y out",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	// The whitelists are input options: they must come before -i.
	if slices.Index(args, "-format_whitelist") > slices.Index(args, "-i") || slices.Index(args, "-protocol_whitelist") > slices.Index(args, "-i") {
		t.Errorf("whitelists after -i: %s", joined)
	}
	formats := strings.Split(args[slices.Index(args, "-format_whitelist")+1], ",")
	for _, bad := range []string{"hls", "concat", "image2", "lavfi", "tee"} {
		if slices.Contains(formats, bad) {
			t.Errorf("format whitelist allows %s", bad)
		}
	}
	if slices.Contains(ConvertArgs("in", "out", 0), "-t") {
		t.Error("no limit should mean no -t")
	}
}

func TestCheckContentType(t *testing.T) {
	for _, ok := range []string{"", "audio/mpeg", "audio/x-wav", "video/mp4", "video/webm", "application/octet-stream",
		"binary/octet-stream", "application/ogg", "Audio/MPEG", "audio/mpeg; charset=binary"} {
		if err := CheckContentType(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for typ, want := range map[string]Kind{
		"text/html": KindPage, "TEXT/HTML; charset=utf-8": KindPage, "application/xhtml+xml": KindPage,
		"text/plain": KindNotMedia, "application/zip": KindNotMedia, "application/x-7z-compressed": KindNotMedia,
		"application/json": KindNotMedia, "image/png": KindNotMedia, "application/pdf": KindNotMedia, ";;": KindNotMedia,
	} {
		if got := kindOf(CheckContentType(typ)); got != want {
			t.Errorf("%q: kind %v, want %v", typ, got, want)
		}
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8:443": true, "162.159.128.233:443": true, "[2606:4700::1]:443": true,
		"127.0.0.1:80": false, "10.1.2.3:80": false, "172.16.0.1:80": false, "192.168.1.1:80": false,
		"169.254.169.254:80": false, "0.0.0.0:80": false, "100.64.0.1:80": false, "255.255.255.255:80": false,
		"224.0.0.1:80": false, "240.0.0.1:80": false, "198.18.0.1:80": false,
		"[::1]:80": false, "[::]:80": false, "[fe80::1]:80": false, "[fc00::1]:80": false, "[fd12::1]:80": false,
		"[ff02::1]:80": false, "[::ffff:127.0.0.1]:80": false, "[::ffff:10.0.0.1]:80": false,
		"[::ffff:8.8.8.8]:80": true, "[64:ff9b::a00:1]:80": false, "[2002:a00:1::]:80": false, "[2001::1]:80": false,
	} {
		if got := Public(netip.MustParseAddrPort(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The real client refuses local servers, and redirects to them.
func TestClientRefusesPrivateAddresses(t *testing.T) {
	f := newFetcher(t, &fakeFFmpeg{}, func(o *Options) { o.Client = NewClient() })
	if _, err := f.Fetch(context.Background(), NewKey(), Source{URL: serve(t, "audio/mpeg", "x")}); kindOf(err) != KindBlocked {
		t.Fatalf("local server: err = %v", err)
	}

	inside := serve(t, "audio/mpeg", "secret")
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inside, http.StatusFound)
	}))
	t.Cleanup(front.Close)
	frontAddr := netip.MustParseAddrPort(strings.TrimPrefix(front.URL, "http://"))
	// Only the front server counts as public; the redirect target doesn't.
	f = newFetcher(t, &fakeFFmpeg{}, func(o *Options) { o.Client = newClient(func(ap netip.AddrPort) bool { return ap == frontAddr }) })
	if _, err := f.Fetch(context.Background(), NewKey(), Source{URL: front.URL}); kindOf(err) != KindBlocked {
		t.Fatalf("redirect to a private address: err = %v", err)
	}
}

func TestClientLimitsRedirects(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.URL+r.URL.Path+"x", http.StatusFound)
	}))
	t.Cleanup(s.Close)
	f := newFetcher(t, &fakeFFmpeg{}, func(o *Options) { o.Client = newClient(func(netip.AddrPort) bool { return true }) })
	if _, err := f.Fetch(context.Background(), NewKey(), Source{URL: s.URL + "/"}); kindOf(err) != KindUnreachable || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("err = %v", err)
	}
}

func TestSweep(t *testing.T) {
	f := newFetcher(t, &fakeFFmpeg{}, nil)
	now := time.Now()
	f.now = func() time.Time { return now }
	write := func(name string, age time.Duration) {
		p := filepath.Join(f.opts.Dir, name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("file-aa.opus", time.Hour) // in use
	write("file-bb.opus", time.Hour) // done with
	write("file-bb.in", time.Hour)
	write("file-cc.part", time.Second) // too new to judge
	f.Sweep(map[string]bool{"file-aa": true})
	var left []string
	entries, _ := os.ReadDir(f.opts.Dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"file-aa.opus", "file-cc.part"}) {
		t.Errorf("left %v", left)
	}

	f.Sweep(nil) // as at startup: everything goes
	if entries, _ := os.ReadDir(f.opts.Dir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestNewEmptiesDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file-aa.opus"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Dir: dir, MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	if _, err := New(Options{Dir: dir}); err == nil {
		t.Error("MaxBytes 0 accepted")
	}
}

func TestTitle(t *testing.T) {
	long := strings.Repeat("a", 150)
	for src, want := range map[Source]string{
		{Name: "song.mp3", URL: "https://cdn/x.mp3"}:                   "song.mp3",
		{URL: "https://archive.org/download/item/Track%2001.flac?x=1"}: "Track 01.flac",
		{URL: "https://example.com/"}:                                  "",
		{URL: "https://example.com"}:                                   "",
		{Name: " bad\x00\nname.mp3 "}:                                  "badname.mp3",
		{Name: long}:                                                   strings.Repeat("a", 99) + "…",
	} {
		if got := Title(src); got != want {
			t.Errorf("Title(%+v) = %q, want %q", src, got, want)
		}
	}
}

func TestUserMessages(t *testing.T) {
	for e, want := range map[*Error]string{
		{Kind: KindTooBig, MaxBytes: 50 << 20}:        "That file is over the 50 MB limit.",
		{Kind: KindTooBig, MaxBytes: 50_000_000}:      "That file is over the 47.7 MB limit.",
		{Kind: KindTooLong, MaxLen: 10 * time.Minute}: "That file is longer than the 10-minute limit.",
		{Kind: KindHTTP, Status: 403}:                 "The link answered with an error (HTTP 403).",
		{Kind: KindPage}:                              "That link is a web page, not an audio or video file. Link straight to the file.",
	} {
		if got := e.UserMessage(); got != want {
			t.Errorf("%v: %q, want %q", e.Kind, got, want)
		}
	}
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, name string, args []string) ([]byte, []byte, error)

func (f RunnerFunc) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	return f(ctx, name, args)
}

// Attachment links are signed: no error or log line may contain one.
func TestErrorsNeverHoldTheLink(t *testing.T) {
	const secret = "attachments/1/2/secret.mp3?ex=SIGNATURE"
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(slow.Close)
	var front *httptest.Server
	front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, front.URL+"/"+secret, http.StatusFound) // loops: too many redirects
	}))
	t.Cleanup(front.Close)
	for name, c := range map[string]struct {
		url    string
		client *http.Client
	}{
		"unreachable":    {"http://127.0.0.1:1/" + secret, http.DefaultClient},
		"no such host":   {"http://no-such-host.invalid/" + secret, http.DefaultClient},
		"blocked":        {slow.URL + "/" + secret, NewClient()},
		"timeout":        {slow.URL + "/" + secret, http.DefaultClient},
		"redirect loop":  {front.URL + "/" + secret, newClient(func(netip.AddrPort) bool { return true })},
		"bad link chars": {"http://example.com/" + secret + "%zz", http.DefaultClient},
	} {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			f := newFetcher(t, &fakeFFmpeg{}, func(o *Options) {
				o.Client, o.DownloadTimeout = c.client, 200*time.Millisecond
				o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			})
			_, err := f.Fetch(context.Background(), NewKey(), Source{URL: c.url, Name: "secret.mp3"})
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "SIGNATURE") || strings.Contains(logs.String(), "SIGNATURE") {
				t.Errorf("link leaked:\nerr: %v\nlog: %s", err, logs.String())
			}
		})
	}
}

// lockedBuffer is a log sink that concurrent fetches can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A file whose track was skipped or stopped stops at once, wherever it is:
// downloading, converting, or waiting for the slot.
func TestFetchStopsWhenTrackGone(t *testing.T) {
	var gone sync.Map // keys no longer queued
	var logs lockedBuffer
	active := func(o *Options) {
		o.Active = func(key string) bool { _, g := gone.Load(key); return !g }
		o.ActiveInterval = 10 * time.Millisecond
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	}
	// A stop isn't a failure, so it mustn't log one.
	t.Cleanup(func() {
		if s := logs.String(); strings.Contains(s, "level=WARN") {
			t.Errorf("a stop logged a warning:\n%s", s)
		}
	})

	t.Run("downloading", func(t *testing.T) {
		started := make(chan struct{})
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "x")
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}))
		t.Cleanup(slow.Close)
		f := newFetcher(t, &fakeFFmpeg{}, active)
		key := NewKey()
		go func() { <-started; gone.Store(key, true) }()
		if _, err := f.Fetch(context.Background(), key, Source{URL: slow.URL}); !errors.Is(err, ErrGone) {
			t.Fatalf("err = %v, want ErrGone", err)
		}
		if entries, _ := os.ReadDir(f.opts.Dir); len(entries) != 0 {
			t.Errorf("left behind %v", entries)
		}
	})

	t.Run("converting, then the next file", func(t *testing.T) {
		running := make(chan struct{})
		k1, k2 := NewKey(), NewKey()
		ff := RunnerFunc(func(ctx context.Context, _ string, args []string) ([]byte, []byte, error) {
			if strings.Contains(args[len(args)-1], k1) { // the first file's conversion hangs
				close(running)
				<-ctx.Done() // until ffmpeg is killed with the context
				return nil, nil, ctx.Err()
			}
			return (&fakeFFmpeg{}).Run(ctx, "", args)
		})
		f := newFetcher(t, ff, active)
		url := serve(t, "audio/mpeg", "x")
		errs := make(chan error, 1)
		go func() { _, err := f.Fetch(context.Background(), k1, Source{URL: url}); errs <- err }()
		<-running
		gone.Store(k1, true)
		start := time.Now()
		if _, err := f.Fetch(context.Background(), k2, Source{URL: url}); err != nil {
			t.Fatalf("next file: %v", err)
		}
		if err := <-errs; !errors.Is(err, ErrGone) {
			t.Errorf("stopped file: err = %v, want ErrGone", err)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Errorf("the next file waited %v for the slot", waited)
		}
	})

	t.Run("waiting for the slot", func(t *testing.T) {
		hold, holding, held := make(chan struct{}), make(chan struct{}), make(chan struct{})
		ff := RunnerFunc(func(ctx context.Context, _ string, args []string) ([]byte, []byte, error) {
			close(holding)
			<-hold
			return (&fakeFFmpeg{}).Run(ctx, "", args)
		})
		f := newFetcher(t, ff, active)
		url := serve(t, "audio/mpeg", "x")
		go func() { // holds the slot
			defer close(held)
			f.Fetch(context.Background(), NewKey(), Source{URL: url})
		}()
		// Let it finish before the test's folder is removed.
		defer func() { close(hold); <-held }()
		<-holding
		waiting := NewKey()
		gone.Store(waiting, true)
		if _, err := f.Fetch(context.Background(), waiting, Source{URL: url}); !errors.Is(err, ErrGone) {
			t.Fatalf("err = %v, want ErrGone", err)
		}
	})
}
