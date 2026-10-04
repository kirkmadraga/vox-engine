package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Runner runs a program with arguments (never through a shell).
type Runner interface {
	Run(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error)
}

// LineRunner is a Runner that can also report each stdout line as it arrives
// (for timing yt-dlp's stages). Runners without it are timed as a whole.
type LineRunner interface {
	RunLines(ctx context.Context, name string, args []string, onLine func(line string)) (stdout, stderr []byte, err error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

func (r ExecRunner) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	return r.RunLines(ctx, name, args, nil)
}

// RunLines is Run, calling onLine (if not nil) for each complete stdout line
// as soon as it is written.
func (ExecRunner) RunLines(ctx context.Context, name string, args []string, onLine func(string)) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// yt-dlp is Python: without this its stdout is block-buffered on a pipe,
	// and every line would arrive at the end.
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	out := &lineWriter{onLine: onLine}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &errb
	cmd.WaitDelay = 5 * time.Second // don't hang on children holding the pipes after a kill
	err := cmd.Run()
	return out.all.Bytes(), errb.Bytes(), err
}

// lineWriter keeps everything written and calls onLine per complete line.
type lineWriter struct {
	all     bytes.Buffer
	partial []byte
	onLine  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.all.Write(p)
	if w.onLine == nil {
		return len(p), nil
	}
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.onLine(strings.TrimRight(string(w.partial[:i]), "\r"))
		w.partial = w.partial[i+1:]
	}
}

// Downloader fetches a video's audio as Ogg Opus using yt-dlp (and ffmpeg,
// which yt-dlp calls to remux or, rarely, re-encode).
type Downloader struct {
	Runner         Runner
	Path           string           // yt-dlp executable
	FFmpegLocation string           // optional: ffmpeg binary or its directory
	JSRuntime      string           // optional: passed as --js-runtimes (e.g. "node")
	Cookies        string           // optional: Netscape cookies.txt; each run gets a private copy, saved back on success
	MaxDuration    time.Duration    // 0 = no limit
	Timeout        time.Duration    // whole download; 0 = DefaultTimeout
	Lock           sync.Locker      // optional: guards Cookies while it's copied or replaced; share with Searcher.Lock
	Logger         *slog.Logger     // optional: timing per download, and cookies that couldn't be saved back
	Now            func() time.Time // nil = time.Now; tests replace it
	Recent         *Recent          // optional: remembers the last download (for debug)
}

// DefaultTimeout bounds one download.
const DefaultTimeout = 5 * time.Minute

// Output markers printed by yt-dlp via --print, so we can parse stdout reliably.
const (
	metaMarker  = "VOXMETA "  // info extracted (incl. YouTube's JS challenge); download starts next
	stageMarker = "VOXSTAGE " // "download" (transfer starts) or "convert" (transfer done)
	fileMarker  = "VOXFILE "  // final file in place
)

// Args returns the yt-dlp arguments for downloading id into dir.
func (d Downloader) Args(id, dir string) []string {
	args := []string{
		"--no-playlist",
		"--no-progress",
		"--no-simulate",
		"--no-mtime",
		"--socket-timeout", "20",
		"--retries", "2",
		"--sleep-requests", "1", // pause between yt-dlp's own requests: gentler on YouTube
		"-f", "bestaudio[acodec=opus]/bestaudio",
		"-x", "--audio-format", "opus",
		"--paths", dir,
		"-o", "%(id)s.%(ext)s",
		"--print", "pre_process:" + metaMarker + "%(is_live)s %(duration)s %(title)s",
		"--print", "before_dl:" + stageMarker + "download",
		"--print", "post_process:" + stageMarker + "convert",
		"--print", "after_move:" + fileMarker + "%(filepath)s",
	}
	filter := "!is_live"
	if d.MaxDuration > 0 {
		filter += fmt.Sprintf(" & duration <= %d", int(d.MaxDuration.Seconds()))
	}
	args = append(args, "--match-filter", filter)
	if d.FFmpegLocation != "" {
		args = append(args, "--ffmpeg-location", d.FFmpegLocation)
	}
	if d.JSRuntime != "" {
		args = append(args, "--js-runtimes", d.JSRuntime)
	}
	if d.Cookies != "" {
		args = append(args, "--cookies", d.Cookies)
	}
	return append(args, VideoURL(id))
}

// Result is a finished download.
type Result struct {
	Path     string
	Title    string        // may be empty
	Duration time.Duration // 0 if unknown
}

// Fetch downloads id's audio into dir (which should be empty and private to
// this call) and returns the resulting .opus file and the video's metadata.
func (d Downloader) Fetch(ctx context.Context, id, dir string) (Result, error) {
	t := newStageTimer(d.Now)
	res, err := d.runFetch(ctx, id, dir, t)
	d.Recent.setDownload(t.run(err))
	if d.Logger != nil {
		d.Logger.Info("ytdlp: download timing", t.attrs(id, err)...)
	}
	return res, err
}

// Version asks yt-dlp for its version (e.g. "2026.09.27"). Starting yt-dlp
// can take seconds on a slow CPU; VersionCache keeps the answer.
func (d Downloader) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, _, err := d.Runner.Run(ctx, d.Path, []string{"--version"})
	if err != nil {
		return "", fmt.Errorf("yt-dlp --version: %w", err)
	}
	v, _, _ := strings.Cut(strings.TrimSpace(string(stdout)), "\n")
	if v == "" {
		return "", errors.New("yt-dlp --version printed nothing")
	}
	return truncate(v, 40), nil
}

func (d Downloader) runFetch(ctx context.Context, id, dir string, t *stageTimer) (Result, error) {
	if !ValidID(id) {
		return Result{}, &Error{Kind: KindInvalid, Detail: "invalid video id " + strconv.Quote(id)}
	}
	run := d // run.Cookies is this run's private copy
	if d.Cookies != "" {
		cookies, err := checkoutCookies(d.Cookies, d.Lock)
		if err != nil {
			return Result{}, &Error{Kind: KindFailed, Detail: err.Error()}
		}
		defer os.Remove(cookies) // already gone if it was saved back
		run.Cookies = cookies
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	t.begin()
	var stdout, stderr []byte
	var err error
	if lr, ok := d.Runner.(LineRunner); ok {
		stdout, stderr, err = lr.RunLines(ctx, d.Path, run.Args(id, dir), t.line)
	} else {
		stdout, stderr, err = d.Runner.Run(ctx, d.Path, run.Args(id, dir))
	}
	t.end()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, &Error{Kind: KindTimeout, Detail: fmt.Sprintf("yt-dlp timed out after %v", timeout)}
		}
		return Result{}, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Result{}, &Error{Kind: KindMissingTool, Detail: err.Error()}
		}
		yerr := classify(string(stderr), err)
		if yerr.Kind == KindBotCheck && d.Cookies != "" {
			yerr.Detail += " (cookies are configured; they may have expired: re-export them)"
		}
		return Result{}, yerr
	}
	if d.Cookies != "" {
		// yt-dlp finished normally, so its copy holds the freshest cookies.
		if err := checkinCookies(run.Cookies, d.Cookies, d.Lock); err != nil && d.Logger != nil {
			d.Logger.Warn("ytdlp: couldn't save refreshed cookies", "err", err)
		}
	}

	var meta, file string
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, metaMarker):
			meta = strings.TrimPrefix(line, metaMarker)
		case strings.HasPrefix(line, fileMarker):
			file = strings.TrimPrefix(line, fileMarker)
		}
	}
	// VOXMETA <is_live> <duration seconds or NA> <title, may contain spaces>
	isLive, rest, _ := strings.Cut(meta, " ")
	duration, title, _ := strings.Cut(rest, " ")
	secs, durErr := strconv.ParseFloat(duration, 64)
	if file != "" {
		r := Result{Path: file, Title: strings.TrimSpace(title)}
		if durErr == nil && secs > 0 {
			r.Duration = time.Duration(secs * float64(time.Second))
		}
		return r, nil
	}
	// No file: the match filter skipped it. The metadata says why.
	if isLive == "True" {
		return Result{}, &Error{Kind: KindLive, Detail: "live stream"}
	}
	if durErr == nil && d.MaxDuration > 0 && secs > d.MaxDuration.Seconds() {
		return Result{}, &Error{Kind: KindTooLong, Detail: fmt.Sprintf("duration %.0fs exceeds %v", secs, d.MaxDuration), Limit: d.MaxDuration}
	}
	return Result{}, &Error{Kind: KindFailed, Detail: "yt-dlp produced no file; stdout: " + truncate(string(stdout), 500) + " stderr: " + truncate(string(stderr), 500)}
}

// truncate cuts s to at most n characters (never inside one), marking the cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
