package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeCookies creates a cookies file with one cookie line and returns its path.
func writeCookies(t *testing.T, cookie string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(cookieFile(cookie)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cookieFile(cookie string) string {
	return "# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t0\t" + cookie + "\n"
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noCopiesLeft fails if any private cookie copy remains next to path.
func noCopiesLeft(t *testing.T, path string) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), cookieCopyPattern)); len(left) > 0 {
		t.Errorf("cookie copies left behind: %v", left)
	}
}

// cookieRunner acts like yt-dlp with --cookies: it reads the cookies file it
// is given and writes new content back to it.
type cookieRunner struct {
	stdout  string
	err     error
	write   string        // written to the --cookies file ("" = leave it)
	block   bool          // wait for ctx instead of finishing
	release chan struct{} // if set, wait for it before finishing
	started chan struct{} // if set, closed once running

	got     string      // the --cookies argument
	content string      // what the file held when yt-dlp started
	mode    os.FileMode // its permissions
}

func (r *cookieRunner) Run(ctx context.Context, _ string, args []string) ([]byte, []byte, error) {
	if i := slices.Index(args, "--cookies"); i >= 0 {
		r.got = args[i+1]
		if b, err := os.ReadFile(r.got); err == nil {
			r.content = string(b)
		}
		if fi, err := os.Stat(r.got); err == nil {
			r.mode = fi.Mode().Perm()
		}
		if r.write != "" {
			os.WriteFile(r.got, []byte(r.write), 0o600)
		}
	}
	if r.started != nil {
		close(r.started)
	}
	if r.release != nil {
		<-r.release
	}
	if r.block {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return []byte(r.stdout), nil, r.err
}

const doneFile = "VOXFILE /tmp/x.opus\n"

// yt-dlp only ever sees a private copy, next to the real file, holding the same cookies.
func checkCopy(t *testing.T, r *cookieRunner, real string) {
	t.Helper()
	if r.got == "" || r.got == real || filepath.Dir(r.got) != filepath.Dir(real) {
		t.Errorf("--cookies = %q, want a copy next to %q", r.got, real)
	}
	if r.content != cookieFile("SID=old") {
		t.Errorf("copy held %q", r.content)
	}
	if runtime.GOOS != "windows" && r.mode != 0o600 {
		t.Errorf("copy mode = %v, want 0600", r.mode)
	}
}

func TestDownloadSavesRefreshedCookies(t *testing.T) {
	real := writeCookies(t, "SID=old")
	r := &cookieRunner{stdout: doneFile, write: cookieFile("SID=new")}
	if _, err := (Downloader{Runner: r, Cookies: real, Lock: &sync.Mutex{}}).Fetch(context.Background(), vid, "d"); err != nil {
		t.Fatal(err)
	}
	checkCopy(t, r, real)
	if got := readFile(t, real); got != cookieFile("SID=new") {
		t.Errorf("real file = %q, want the refreshed cookies", got)
	}
	noCopiesLeft(t, real)
}

func TestFailedDownloadKeepsCookies(t *testing.T) {
	cases := map[string]*cookieRunner{
		"yt-dlp error": {err: exitErr, write: cookieFile("SID=new")},
		"timeout":      {block: true, write: cookieFile("SID=new")},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			real := writeCookies(t, "SID=old")
			d := Downloader{Runner: r, Cookies: real, Timeout: 20 * time.Millisecond}
			if _, err := d.Fetch(context.Background(), vid, "d"); err == nil {
				t.Fatal("want an error")
			}
			if got := readFile(t, real); got != cookieFile("SID=old") {
				t.Errorf("real file = %q, want it untouched", got)
			}
			noCopiesLeft(t, real)
		})
	}
}

func TestInvalidRefreshKeepsCookies(t *testing.T) {
	real := writeCookies(t, "SID=old")
	var logs bytes.Buffer
	d := Downloader{
		Runner:  &cookieRunner{stdout: doneFile, write: "garbage"},
		Cookies: real,
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	}
	if _, err := d.Fetch(context.Background(), vid, "d"); err != nil {
		t.Fatal(err) // the download itself succeeded
	}
	if got := readFile(t, real); got != cookieFile("SID=old") {
		t.Errorf("real file = %q, want it untouched", got)
	}
	if !strings.Contains(logs.String(), "couldn't save refreshed cookies") {
		t.Errorf("no warning logged: %q", logs.String())
	}
	noCopiesLeft(t, real)
}

func TestSearchNeverSavesCookies(t *testing.T) {
	real := writeCookies(t, "SID=old")
	r := &cookieRunner{stdout: zooLine, write: cookieFile("SID=new")}
	if _, err := (&Searcher{Runner: r, Cookies: real}).SearchN(context.Background(), "q", 1); err != nil {
		t.Fatal(err)
	}
	checkCopy(t, r, real)
	if got := readFile(t, real); got != cookieFile("SID=old") {
		t.Errorf("real file = %q, want it untouched", got)
	}
	noCopiesLeft(t, real)
}

func TestMissingCookiesFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")
	r := &cookieRunner{}
	var yerr *Error
	if _, err := (Downloader{Runner: r, Cookies: missing}).Fetch(context.Background(), vid, "d"); !errors.As(err, &yerr) || !strings.Contains(err.Error(), "read cookies") {
		t.Errorf("download: %v", err)
	}
	if _, err := (&Searcher{Runner: r, Cookies: missing}).SearchN(context.Background(), "q", 1); !errors.As(err, &yerr) || !strings.Contains(err.Error(), "read cookies") {
		t.Errorf("search: %v", err)
	}
	if r.got != "" {
		t.Error("yt-dlp must not run without its cookies")
	}
}

// countingLocker counts Lock calls.
type countingLocker struct {
	sync.Mutex
	locks int
}

func (l *countingLocker) Lock() { l.Mutex.Lock(); l.locks++ }

// Without cookies nothing changes: no copies, no --cookies, no locking.
func TestNoCookiesNoCopies(t *testing.T) {
	lock := &countingLocker{}
	dr := &cookieRunner{stdout: doneFile}
	if _, err := (Downloader{Runner: dr, Lock: lock}).Fetch(context.Background(), vid, "d"); err != nil {
		t.Fatal(err)
	}
	sr := &cookieRunner{stdout: zooLine}
	if _, err := (&Searcher{Runner: sr, Lock: lock}).SearchN(context.Background(), "q", 1); err != nil {
		t.Fatal(err)
	}
	if dr.got != "" || sr.got != "" || lock.locks != 0 {
		t.Errorf("cookies %q/%q, locked %d times", dr.got, sr.got, lock.locks)
	}
}

// A search runs while a download is still running (both use cookies).
func TestSearchRunsDuringDownload(t *testing.T) {
	real := writeCookies(t, "SID=old")
	lock := &sync.Mutex{}
	dr := &cookieRunner{stdout: doneFile, write: cookieFile("SID=new"), started: make(chan struct{}), release: make(chan struct{})}
	downloaded := make(chan error)
	go func() {
		_, err := Downloader{Runner: dr, Cookies: real, Lock: lock}.Fetch(context.Background(), vid, "d")
		downloaded <- err
	}()
	<-dr.started

	searched := make(chan error)
	go func() {
		_, err := (&Searcher{Runner: &cookieRunner{stdout: zooLine}, Cookies: real, Lock: lock}).SearchN(context.Background(), "q", 1)
		searched <- err
	}()
	select {
	case err := <-searched:
		if err != nil {
			t.Errorf("search: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search waited for the download")
	}

	close(dr.release)
	if err := <-downloaded; err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, real); got != cookieFile("SID=new") {
		t.Errorf("real file = %q, want the download's refresh", got)
	}
	noCopiesLeft(t, real)
}

func TestRemoveStaleCookieCopies(t *testing.T) {
	real := writeCookies(t, "SID=old")
	dir := filepath.Dir(real)
	for _, name := range []string{".ytdlp-cookies-123.txt", ".ytdlp-cookies-abc.txt", "other.txt"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
	if err := RemoveStaleCookieCopies(real); err != nil {
		t.Fatal(err)
	}
	noCopiesLeft(t, real)
	for _, keep := range []string{real, filepath.Join(dir, "other.txt")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed", keep)
		}
	}
}
