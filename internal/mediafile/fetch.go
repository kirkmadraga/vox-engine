// Package mediafile plays audio and video files: Discord attachments and
// direct links to files. Each is downloaded, converted by ffmpeg to the same
// Ogg Opus the audio cache holds, checked, and kept only while it's queued or
// playing.
package mediafile

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kirkmadraga/vox-engine/internal/voice"
)

// Defaults for Options.
const (
	DefaultDownloadTimeout = 3 * time.Minute
	DefaultConvertTimeout  = 10 * time.Minute
	DefaultSweepInterval   = time.Minute
	DefaultActiveInterval  = 2 * time.Second
)

// ErrGone means a fetch stopped because its track left the queue (skipped
// or stopped): nobody is waiting for the file any more.
var ErrGone = errors.New("file no longer queued")

// sweepGrace keeps files this young from Sweep: a track's key is in use from
// the moment it's queued, so this only covers clock and timing slack.
const sweepGrace = time.Minute

// KeyPrefix starts every file key, so keys never collide with video IDs.
const KeyPrefix = "file-"

const maxTitleRunes = 100

// Runner runs a program. ytdlp.ExecRunner implements it.
type Runner interface {
	Run(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error)
}

// Source is a file to play.
type Source struct {
	URL  string // http(s); never logged for attachments (Discord's signed links)
	Name string // file name, used as the title; "" = taken from the URL
	Size int64  // bytes, when known in advance (attachments); 0 = unknown
}

// Result is a converted file, ready to play.
type Result struct {
	Path     string
	Title    string
	Duration time.Duration
}

// Options configures a Fetcher.
type Options struct {
	Dir             string       // where files are kept; emptied by New
	FFmpeg          string       // ffmpeg's path
	Runner          Runner       // runs ffmpeg
	Client          *http.Client // nil = NewClient()
	MaxBytes        int64        // largest file accepted; must be positive
	MaxDuration     time.Duration
	DownloadTimeout time.Duration // 0 = DefaultDownloadTimeout
	ConvertTimeout  time.Duration // 0 = DefaultConvertTimeout
	// Active reports whether key's track is still queued or playing; a fetch
	// whose track is gone stops (download or ffmpeg alike), freeing the slot
	// for the next file. nil = never stop. Asked every ActiveInterval.
	Active         func(key string) bool
	ActiveInterval time.Duration // 0 = DefaultActiveInterval
	Logger         *slog.Logger
}

// Fetcher downloads and converts files, one at a time.
type Fetcher struct {
	opts  Options
	slots chan struct{}
	now   func() time.Time
}

// New builds a Fetcher, creating opts.Dir and removing anything left in it
// (files of tracks queued when the bot last stopped).
func New(opts Options) (*Fetcher, error) {
	if opts.MaxBytes <= 0 {
		return nil, errors.New("mediafile: MaxBytes must be positive")
	}
	if opts.Client == nil {
		opts.Client = NewClient()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	opts.DownloadTimeout = cmp.Or(opts.DownloadTimeout, DefaultDownloadTimeout)
	opts.ConvertTimeout = cmp.Or(opts.ConvertTimeout, DefaultConvertTimeout)
	opts.ActiveInterval = cmp.Or(opts.ActiveInterval, DefaultActiveInterval)
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}
	f := &Fetcher{opts: opts, slots: make(chan struct{}, 1), now: time.Now}
	f.Sweep(nil)
	return f, nil
}

// NewKey returns a fresh key for one queued file.
func NewKey() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return KeyPrefix + hex.EncodeToString(b)
}

// Fetch downloads src and converts it into the file for key.
func (f *Fetcher) Fetch(ctx context.Context, key string, src Source) (Result, error) {
	if !validKey(key) {
		return Result{}, &Error{Kind: KindFailed, Detail: fmt.Sprintf("invalid key %q", key)}
	}
	if src.Size > f.opts.MaxBytes {
		return Result{}, f.tooBig(src.Size)
	}
	ctx, stop := f.watch(ctx, key)
	defer stop()
	res, err := f.fetch(ctx, key, src)
	if errors.Is(context.Cause(ctx), ErrGone) {
		f.opts.Logger.Info("file: stopped, its track left the queue", "file", key)
		return Result{}, ErrGone
	}
	return res, err
}

// watch returns ctx cancelled with ErrGone once key's track is no longer
// queued or playing.
func (f *Fetcher) watch(ctx context.Context, key string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	if f.opts.Active == nil {
		return ctx, func() { cancel(nil) }
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(f.opts.ActiveInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !f.opts.Active(key) {
					cancel(ErrGone)
					return
				}
			}
		}
	}()
	return ctx, func() { close(done); cancel(nil) }
}

func (f *Fetcher) fetch(ctx context.Context, key string, src Source) (Result, error) {
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	in := filepath.Join(f.opts.Dir, key+".in")
	part := filepath.Join(f.opts.Dir, key+".part")
	out := filepath.Join(f.opts.Dir, key+".opus")
	defer os.Remove(in)
	defer os.Remove(part)

	log := f.opts.Logger.With("file", key)
	// warn skips failures caused by the track leaving the queue; Fetch logs those.
	warn := func(msg string, err error) {
		if !errors.Is(context.Cause(ctx), ErrGone) {
			log.Warn(msg, "err", err)
		}
	}
	start := f.now()
	n, err := f.download(ctx, src.URL, in)
	if err != nil {
		warn("file: download failed", err)
		return Result{}, err
	}
	downloaded := f.now()
	if err := f.convert(ctx, in, part); err != nil {
		warn("file: conversion failed", err)
		return Result{}, err
	}
	d, err := voice.InspectFile(part)
	if err != nil {
		log.Warn("file: converted audio rejected", "err", err)
		return Result{}, &Error{Kind: KindNoAudio, Detail: "converted audio is unusable: " + err.Error()}
	}
	if f.opts.MaxDuration > 0 && d > f.opts.MaxDuration {
		return Result{}, &Error{Kind: KindTooLong, Detail: fmt.Sprintf("longer than %v", f.opts.MaxDuration), MaxLen: f.opts.MaxDuration}
	}
	if err := os.Rename(part, out); err != nil {
		return Result{}, fmt.Errorf("move converted file: %w", err)
	}
	log.Info("file: ready", "bytes", n, "duration", d,
		"download", downloaded.Sub(start).Round(time.Millisecond), "convert", f.now().Sub(downloaded).Round(time.Millisecond))
	return Result{Path: out, Title: Title(src), Duration: d}, nil
}

func (f *Fetcher) tooBig(size int64) *Error {
	return &Error{Kind: KindTooBig, Detail: fmt.Sprintf("%d bytes, limit %d", size, f.opts.MaxBytes), MaxBytes: f.opts.MaxBytes}
}

// download saves rawURL to dst, refusing pages and anything over MaxBytes.
func (f *Fetcher) download(ctx context.Context, rawURL, dst string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, f.opts.DownloadTimeout)
	defer cancel()
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return 0, &Error{Kind: KindBadLink, Detail: "not an http(s) link"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, &Error{Kind: KindBadLink, Detail: "unusable link"} // err quotes the link
	}
	req.Header.Set("User-Agent", "vox-engine (+https://github.com/kirkmadraga/vox-engine)")
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		return 0, transferError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, &Error{Kind: KindHTTP, Status: resp.StatusCode, Detail: resp.Status}
	}
	if err := CheckContentType(resp.Header.Get("Content-Type")); err != nil {
		return 0, err
	}
	if resp.ContentLength > f.opts.MaxBytes {
		return 0, f.tooBig(resp.ContentLength)
	}

	file, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("create download file: %w", err)
	}
	n, err := io.Copy(file, io.LimitReader(resp.Body, f.opts.MaxBytes+1))
	if cerr := file.Close(); err == nil && cerr != nil {
		return n, fmt.Errorf("write download file: %w", cerr)
	}
	switch {
	case err != nil:
		return n, transferError(ctx, err)
	case n > f.opts.MaxBytes:
		return n, &Error{Kind: KindTooBig, Detail: fmt.Sprintf("more than %d bytes", f.opts.MaxBytes), MaxBytes: f.opts.MaxBytes}
	case n == 0:
		return n, &Error{Kind: KindNoAudio, Detail: "empty file"}
	}
	return n, nil
}

// transferError classifies a failed request or body read. The Detail never
// holds the link: an attachment's is signed, and Detail is logged.
func transferError(ctx context.Context, err error) *Error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // its own text is `Get "<link>": ...`
	}
	switch {
	case errors.Is(err, errBlocked):
		return &Error{Kind: KindBlocked, Detail: err.Error()}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Error{Kind: KindTimeout, Detail: err.Error()}
	}
	return &Error{Kind: KindUnreachable, Detail: err.Error()}
}

// CheckContentType accepts what a server says is audio, video, or a generic
// download (which ffmpeg then judges). Web pages, text, archives and the rest
// are refused before anything is downloaded. A missing type is allowed.
func CheckContentType(header string) error {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	t, _, err := mime.ParseMediaType(header)
	if err != nil {
		return &Error{Kind: KindNotMedia, Detail: "bad content type " + strconv.Quote(header)}
	}
	switch {
	case strings.HasPrefix(t, "audio/"), strings.HasPrefix(t, "video/"):
		return nil
	case t == "application/octet-stream", t == "binary/octet-stream", t == "application/ogg",
		t == "application/x-ogg", t == "application/mp4":
		return nil
	case t == "text/html", t == "application/xhtml+xml":
		return &Error{Kind: KindPage, Detail: "content type " + t}
	}
	return &Error{Kind: KindNotMedia, Detail: "content type " + t}
}

// demuxers are the input formats ffmpeg may read: ordinary audio and video
// containers. Playlist and reference formats (hls, concat, ...) are left out,
// as they could make ffmpeg open other files.
var demuxers = strings.Join([]string{
	"mp3", "mov", "mp4", "m4a", "3gp", "3g2", "mj2", "matroska", "webm", "ogg", "wav", "w64", "flac",
	"aac", "aiff", "caf", "asf", "avi", "mpegts", "mpeg", "flv", "amr", "ape", "wv", "tta", "au", "ac3",
	"eac3", "dts", "truehd", "mlp",
}, ",")

// ConvertArgs are ffmpeg's arguments to turn in into Ogg Opus at out: 48 kHz
// stereo in 20 ms packets, the first audio stream only. ffmpeg may read only
// the local file (no network or other protocols), in the formats above.
func ConvertArgs(in, out string, maxLen time.Duration) []string {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file",
		"-format_whitelist", demuxers,
		"-i", in,
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-map_metadata", "-1",
		"-c:a", "libopus", "-b:a", "128k", "-frame_duration", "20", "-application", "audio",
		"-ar", "48000", "-ac", "2",
	}
	if maxLen > 0 {
		// Stop just past the limit; the result is then measured and refused.
		args = append(args, "-t", strconv.Itoa(int(maxLen.Seconds())+1))
	}
	return append(args, "-f", "ogg", "-y", out)
}

func (f *Fetcher) convert(ctx context.Context, in, out string) error {
	ctx, cancel := context.WithTimeout(ctx, f.opts.ConvertTimeout)
	defer cancel()
	_, stderr, err := f.opts.Runner.Run(ctx, f.opts.FFmpeg, ConvertArgs(in, out, f.opts.MaxDuration))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return &Error{Kind: KindMissingTool, Detail: err.Error()}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Error{Kind: KindTimeout, Detail: "ffmpeg: " + err.Error()}
	}
	return &Error{Kind: KindNoAudio, Detail: "ffmpeg: " + truncate(strings.TrimSpace(string(stderr)), 300)}
}

// Sweep removes every file whose key isn't in inUse, except files changed in
// the last minute. nil removes everything.
func (f *Fetcher) Sweep(inUse map[string]bool) {
	entries, err := os.ReadDir(f.opts.Dir)
	if err != nil {
		f.opts.Logger.Warn("file: could not read files dir", "err", err)
		return
	}
	for _, e := range entries {
		key, _, _ := strings.Cut(e.Name(), ".")
		if inUse != nil && inUse[key] {
			continue
		}
		info, err := e.Info()
		if err != nil || (inUse != nil && f.now().Sub(info.ModTime()) < sweepGrace) {
			continue
		}
		p := filepath.Join(f.opts.Dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			f.opts.Logger.Warn("file: could not remove", "path", p, "err", err)
		}
	}
}

// SweepLoop runs Sweep every interval (0 = DefaultSweepInterval) until ctx ends.
func (f *Fetcher) SweepLoop(ctx context.Context, inUse func() map[string]bool, interval time.Duration) {
	t := time.NewTicker(cmp.Or(interval, DefaultSweepInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Sweep(inUse())
		}
	}
}

func validKey(key string) bool {
	rest, ok := strings.CutPrefix(key, KeyPrefix)
	if !ok || rest == "" {
		return false
	}
	return !strings.ContainsFunc(rest, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	})
}

// Title is the name shown for src: its file name, else the last part of its
// link's path, with control characters dropped and at most 100 characters.
func Title(src Source) string {
	name := src.Name
	if name == "" {
		if u, err := url.Parse(src.URL); err == nil {
			name = path.Base(u.Path)
			if name == "/" || name == "." {
				name = ""
			}
		}
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > maxTitleRunes {
		name = string([]rune(name)[:maxTitleRunes-1]) + "…"
	}
	return name
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
