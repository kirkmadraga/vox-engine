//go:build integration

// Live test: real yt-dlp, real network. Run with:
//
//	go test -tags integration ./internal/cache/
//
// Optional environment: YTDLP_PATH (default: yt-dlp on PATH), FFMPEG_PATH,
// YTDLP_JS_RUNTIME (e.g. "node"; YouTube needs a JS runtime).
package cache

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"bot/internal/voice"
	"bot/internal/ytdlp"
)

func liveDownloader(t *testing.T) ytdlp.Downloader {
	t.Helper()
	path := os.Getenv("YTDLP_PATH")
	if path == "" {
		var err error
		if path, err = exec.LookPath("yt-dlp"); err != nil {
			t.Skip("yt-dlp not found")
		}
	}
	return ytdlp.Downloader{
		Runner:         ytdlp.ExecRunner{},
		Path:           path,
		FFmpegLocation: os.Getenv("FFMPEG_PATH"),
		JSRuntime:      os.Getenv("YTDLP_JS_RUNTIME"),
		Timeout:        2 * time.Minute,
	}
}

// "Me at the zoo", 19 s: the first YouTube video, very unlikely to disappear.
const liveID = "jNQXAC9IVRw"

func TestLiveDownloadValidatesAndCaches(t *testing.T) {
	c, dir := newCache(t, liveDownloader(t), func(o *Options) { o.Validate = voice.ValidateFile })

	start := time.Now()
	e, hit, err := c.Get(context.Background(), liveID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	t.Logf("downloaded %q (%v) in %v to %s", e.Title, e.Duration, time.Since(start).Round(time.Millisecond), e.Path)
	if e.Title == "" || e.Duration == 0 {
		t.Error("expected title and duration from yt-dlp")
	}
	if hit {
		t.Error("first Get should be a miss")
	}
	assertOnlyAudio(t, dir, liveID+".json", liveID+".opus")

	if _, hit, err := c.Get(context.Background(), liveID); err != nil || !hit {
		t.Errorf("second Get: hit=%v err=%v", hit, err)
	}
}

func TestLiveDurationLimit(t *testing.T) {
	d := liveDownloader(t)
	d.MaxDuration = 5 * time.Second
	c, dir := newCache(t, d)
	_, _, err := c.Get(context.Background(), liveID)
	var ye *ytdlp.Error
	if !errors.As(err, &ye) || ye.Kind != ytdlp.KindTooLong {
		t.Fatalf("err = %v, want KindTooLong", err)
	}
	assertOnlyAudio(t, dir)
}
