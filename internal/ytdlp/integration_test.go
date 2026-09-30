//go:build integration

// Live test: real yt-dlp, real network. Run with:
//
//	go test -tags integration ./internal/ytdlp/
//
// Optional environment: YTDLP_PATH (default: yt-dlp on PATH), FFMPEG_PATH,
// YTDLP_JS_RUNTIME (e.g. "node"; YouTube needs a JS runtime).
package ytdlp

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

// Real yt-dlp prints the stage markers in order and as each stage happens, so
// the timing log has every phase and they add up to less than the total.
func TestLiveDownloadTiming(t *testing.T) {
	path := os.Getenv("YTDLP_PATH")
	if path == "" {
		var err error
		if path, err = exec.LookPath("yt-dlp"); err != nil {
			t.Skip("yt-dlp not found")
		}
	}
	log, buf := logger()
	d := Downloader{
		Runner: ExecRunner{}, Path: path, Logger: log,
		FFmpegLocation: os.Getenv("FFMPEG_PATH"), JSRuntime: os.Getenv("YTDLP_JS_RUNTIME"),
		Timeout: 2 * time.Minute,
	}
	if _, err := d.Fetch(context.Background(), vid, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Log(buf.String())
	m := regexp.MustCompile(`extract=(\S+) download=(\S+) convert=(\S+) total=(\S+) outcome=ok`).FindStringSubmatch(buf.String())
	if m == nil {
		t.Fatalf("not every phase was timed: %s", buf.String())
	}
	var sum time.Duration
	for _, s := range m[1:4] {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatal(err)
		}
		sum += d
	}
	total, _ := time.ParseDuration(m[4])
	if sum > total+200*time.Millisecond {
		t.Errorf("phases add up to %v, more than the total %v", sum, total)
	}
}
