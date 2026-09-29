package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner runs a program with arguments (never through a shell).
type Runner interface {
	Run(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second // don't hang on children holding the pipes after a kill
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// Downloader fetches a video's audio as Ogg Opus using yt-dlp (and ffmpeg,
// which yt-dlp calls to remux or, rarely, re-encode).
type Downloader struct {
	Runner         Runner
	Path           string        // yt-dlp executable
	FFmpegLocation string        // optional: ffmpeg binary or its directory
	JSRuntime      string        // optional: passed as --js-runtimes (e.g. "node")
	Cookies        string        // optional: Netscape cookies.txt passed as --cookies (yt-dlp also writes it back)
	MaxDuration    time.Duration // 0 = no limit
	Timeout        time.Duration // whole download; 0 = DefaultTimeout
}

// DefaultTimeout bounds one download.
const DefaultTimeout = 5 * time.Minute

// Output markers printed by yt-dlp via --print, so we can parse stdout reliably.
const (
	metaMarker = "VOXMETA "
	fileMarker = "VOXFILE "
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
	if !ValidID(id) {
		return Result{}, &Error{Kind: KindInvalid, Detail: "invalid video id " + strconv.Quote(id)}
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, err := d.Runner.Run(ctx, d.Path, d.Args(id, dir))
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
