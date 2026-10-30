//go:build integration

// Real ffmpeg on generated files. Run with:
//
//	go test -tags integration ./internal/mediafile/
//
// Optional environment: FFMPEG_PATH (default: ffmpeg on PATH).
package mediafile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/voice"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

func ffmpegPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("FFMPEG_PATH"); p != "" {
		return p
	}
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	return p
}

// makeMedia writes a file with ffmpeg's own generators.
func makeMedia(t *testing.T, ffmpeg, dir, name string, args ...string) {
	t.Helper()
	full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...)
	full = append(full, "-y", filepath.Join(dir, name))
	if out, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v\n%s", name, err, out)
	}
}

func TestRealFFmpeg(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	src := t.TempDir()
	sine := func(secs string) []string {
		return []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=" + secs}
	}
	video := []string{"-f", "lavfi", "-i", "testsrc=duration=3:size=64x64:rate=10"}
	makeMedia(t, ffmpeg, src, "a.mp3", sine("3")...)
	makeMedia(t, ffmpeg, src, "a.wav", sine("3")...)
	makeMedia(t, ffmpeg, src, "a.flac", sine("3")...)
	makeMedia(t, ffmpeg, src, "a.m4a", append(sine("3"), "-c:a", "aac")...)
	makeMedia(t, ffmpeg, src, "v.mp4", append(append(video, sine("3")...), "-shortest", "-c:v", "mpeg4", "-c:a", "aac")...)
	makeMedia(t, ffmpeg, src, "v.webm", append(append(video, sine("3")...), "-shortest", "-c:v", "libvpx", "-c:a", "libopus")...)
	makeMedia(t, ffmpeg, src, "silent.mp4", append(video, "-c:v", "mpeg4")...)
	makeMedia(t, ffmpeg, src, "long.mp3", sine("8")...)
	makeMedia(t, ffmpeg, src, "pl.m3u8", append(sine("3"), "-c:a", "aac", "-f", "hls", "-hls_time", "1")...)
	whole, err := os.ReadFile(filepath.Join(src, "a.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "cut.mp3"), whole[:200], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "random.bin"), []byte("this is not media at all, just words"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, filepath.Join(src, filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(server.Close)

	f, err := New(Options{Dir: t.TempDir(), FFmpeg: ffmpeg, Runner: ytdlp.ExecRunner{}, Client: http.DefaultClient,
		MaxBytes: 10 << 20, MaxDuration: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]Kind{
		"a.mp3": -1, "a.wav": -1, "a.flac": -1, "a.m4a": -1, "v.mp4": -1, "v.webm": -1,
		"silent.mp4": KindNoAudio, "cut.mp3": KindNoAudio, "random.bin": KindNoAudio,
		"pl.m3u8":  KindNoAudio, // a playlist: not a format ffmpeg may open here
		"long.mp3": KindTooLong,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := f.Fetch(context.Background(), NewKey(), Source{URL: server.URL + "/" + name})
			if got := kindOf(err); got != want {
				t.Fatalf("err = %v, want kind %v", err, want)
			}
			if err != nil {
				return
			}
			if err := voice.ValidateFile(res.Path); err != nil {
				t.Errorf("result invalid: %v", err)
			}
			if res.Duration < 2900*time.Millisecond || res.Duration > 3100*time.Millisecond {
				t.Errorf("duration = %v, want about 3s", res.Duration)
			}
		})
	}
}
