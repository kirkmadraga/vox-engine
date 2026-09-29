package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	stdout, stderr string
	err            error
	block          bool // wait for ctx instead of returning
	gotName        string
	gotArgs        []string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	f.gotName, f.gotArgs = name, args
	if f.block {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return []byte(f.stdout), []byte(f.stderr), f.err
}

const vid = "jNQXAC9IVRw"

var exitErr = errors.New("exit status 1")

func TestFetchSuccess(t *testing.T) {
	r := &fakeRunner{stdout: "VOXMETA False 19.5 Me at the zoo\nVOXFILE /tmp/dl/jNQXAC9IVRw.opus\n"}
	d := Downloader{Runner: r, Path: "yt-dlp", JSRuntime: "node", FFmpegLocation: "/usr/bin/ffmpeg"}
	res, err := d.Fetch(context.Background(), vid, "/tmp/dl")
	if err != nil || res.Path != "/tmp/dl/jNQXAC9IVRw.opus" {
		t.Fatalf("Fetch = %+v, %v", res, err)
	}
	if res.Title != "Me at the zoo" || res.Duration != 19500*time.Millisecond {
		t.Errorf("metadata = %q, %v", res.Title, res.Duration)
	}
	if r.gotName != "yt-dlp" {
		t.Errorf("ran %q", r.gotName)
	}
	for _, want := range [][]string{
		{"--no-playlist"},
		{"-x", "--audio-format", "opus"},
		{"-f", "bestaudio[acodec=opus]/bestaudio"},
		{"--paths", "/tmp/dl"},
		{"--js-runtimes", "node"},
		{"--ffmpeg-location", "/usr/bin/ffmpeg"},
		{"--match-filter", "!is_live"},
	} {
		if !containsSeq(r.gotArgs, want) {
			t.Errorf("args missing %v: %v", want, r.gotArgs)
		}
	}
	// Only the canonical URL is passed, and it is last.
	if last := r.gotArgs[len(r.gotArgs)-1]; last != "https://www.youtube.com/watch?v=jNQXAC9IVRw" {
		t.Errorf("last arg = %q", last)
	}
}

func TestArgsOptionalFlags(t *testing.T) {
	args := Downloader{MaxDuration: 10 * time.Minute}.Args(vid, "d")
	if !containsSeq(args, []string{"--match-filter", "!is_live & duration <= 600"}) {
		t.Errorf("duration filter missing: %v", args)
	}
	for _, flag := range []string{"--js-runtimes", "--ffmpeg-location"} {
		if slices.Contains(args, flag) {
			t.Errorf("%s must be omitted when not configured", flag)
		}
	}
}

func TestFetchClassifiesErrors(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   Kind
	}{
		{"private", "ERROR: [youtube] abc: Private video. Sign in if you've been granted access to this video", KindPrivate},
		{"age", "ERROR: [youtube] abc: Sign in to confirm your age. This video may be inappropriate for some users.", KindAgeRestricted},
		{"region", "ERROR: [youtube] abc: Video unavailable. The uploader has not made this video available in your country", KindRegionLocked},
		{"members", "ERROR: [youtube] abc: Join this channel to get access to members-only content like this video, and other exclusive perks.", KindMembersOnly},
		{"removed", "ERROR: [youtube] abc: Video unavailable. This video has been removed by the uploader", KindUnavailable},
		{"unavailable", "ERROR: [youtube] abc: Video unavailable", KindUnavailable},
		{"terminated", "ERROR: [youtube] abc: Video unavailable. This video is no longer available because the YouTube account associated with this video has been terminated.", KindUnavailable},
		{"upcoming", "ERROR: [youtube] abc: This live event will begin in 3 hours.", KindLive},
		{"premiere", "ERROR: [youtube] abc: Premieres in 2 days", KindLive},
		{"bot check", "ERROR: [youtube] abc: Sign in to confirm you're not a bot. Use --cookies-from-browser or --cookies for the authentication.", KindBotCheck},
		{"unknown", "ERROR: something new and strange happened", KindFailed},
		{"warnings then error", "WARNING: slow\nWARNING: retrying\nERROR: [youtube] abc: Private video", KindPrivate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Downloader{Runner: &fakeRunner{stderr: c.stderr, err: exitErr}, Path: "yt-dlp"}
			_, err := d.Fetch(context.Background(), vid, "d")
			var ye *Error
			if !errors.As(err, &ye) || ye.Kind != c.want {
				t.Fatalf("err = %v, want kind %d", err, c.want)
			}
			if ye.UserMessage() == "" {
				t.Error("empty user message")
			}
			if !strings.Contains(ye.Error(), "ERROR:") {
				t.Errorf("log detail should carry the ERROR line: %q", ye.Error())
			}
		})
	}
}

func TestFetchSkippedByFilter(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		d := Downloader{Runner: &fakeRunner{stdout: "VOXMETA True NA\n"}, Path: "yt-dlp"}
		_, err := d.Fetch(context.Background(), vid, "d")
		if k := kindOf(err); k != KindLive {
			t.Errorf("kind = %d, err = %v", k, err)
		}
	})
	t.Run("too long", func(t *testing.T) {
		d := Downloader{Runner: &fakeRunner{stdout: "VOXMETA False 7200\n"}, Path: "yt-dlp", MaxDuration: 10 * time.Minute}
		_, err := d.Fetch(context.Background(), vid, "d")
		var ye *Error
		if !errors.As(err, &ye) || ye.Kind != KindTooLong {
			t.Fatalf("err = %v", err)
		}
		if msg := ye.UserMessage(); msg != "That video is longer than the 10-minute limit." {
			t.Errorf("message = %q", msg)
		}
	})
	t.Run("no file, no reason", func(t *testing.T) {
		d := Downloader{Runner: &fakeRunner{stdout: "VOXMETA False 19\n"}, Path: "yt-dlp"}
		if k := kindOf(d.fetch(t)); k != KindFailed {
			t.Errorf("kind = %d", k)
		}
	})
}

func TestFetchTimeout(t *testing.T) {
	d := Downloader{Runner: &fakeRunner{block: true}, Path: "yt-dlp", Timeout: 20 * time.Millisecond}
	if k := kindOf(d.fetch(t)); k != KindTimeout {
		t.Errorf("kind = %d", k)
	}
}

func TestFetchCallerCancelIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := Downloader{Runner: &fakeRunner{block: true}, Path: "yt-dlp"}
	if _, err := d.Fetch(ctx, vid, "d"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFetchMissingTool(t *testing.T) {
	d := Downloader{Runner: &fakeRunner{err: fmt.Errorf("exec: %w", exec.ErrNotFound)}, Path: "yt-dlp"}
	if k := kindOf(d.fetch(t)); k != KindMissingTool {
		t.Errorf("kind = %d", k)
	}
}

func TestFetchRejectsInvalidID(t *testing.T) {
	r := &fakeRunner{}
	d := Downloader{Runner: r, Path: "yt-dlp"}
	if _, err := d.Fetch(context.Background(), "--exec=rm", "d"); kindOf(err) != KindInvalid {
		t.Errorf("err = %v", err)
	}
	if r.gotName != "" {
		t.Error("yt-dlp must not run for an invalid id")
	}
}

func (d Downloader) fetch(t *testing.T) error {
	t.Helper()
	_, err := d.Fetch(context.Background(), vid, "d")
	return err
}

func kindOf(err error) Kind {
	var ye *Error
	if errors.As(err, &ye) {
		return ye.Kind
	}
	return -1
}

func containsSeq(args, seq []string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestFetchMetadataEdgeCases(t *testing.T) {
	for stdout, want := range map[string]Result{
		"VOXMETA False NA Some  Title  \nVOXFILE f.opus\n": {Path: "f.opus", Title: "Some  Title"},
		"VOXMETA False 60\nVOXFILE f.opus\n":               {Path: "f.opus", Duration: time.Minute},
		"VOXFILE f.opus\n":                                 {Path: "f.opus"},
	} {
		d := Downloader{Runner: &fakeRunner{stdout: stdout}, Path: "yt-dlp"}
		got, err := d.Fetch(context.Background(), vid, "d")
		if err != nil || got != want {
			t.Errorf("stdout %q: got %+v, %v; want %+v", stdout, got, err, want)
		}
	}
}

func TestCookiesFlagAndExpiryHint(t *testing.T) {
	args := Downloader{Cookies: "/opt/discord-bot/data/cookies.txt"}.Args(vid, "d")
	if !containsSeq(args, []string{"--cookies", "/opt/discord-bot/data/cookies.txt"}) {
		t.Errorf("--cookies missing: %v", args)
	}
	if slices.Contains(Downloader{}.Args(vid, "d"), "--cookies") {
		t.Error("--cookies must be omitted when not configured")
	}

	botCheck := "ERROR: [youtube] abc: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies"
	withCookies := Downloader{Runner: &fakeRunner{stderr: botCheck, err: exitErr}, Path: "yt-dlp", Cookies: "c.txt"}
	_, err := withCookies.Fetch(context.Background(), vid, "d")
	var ye *Error
	if !errors.As(err, &ye) || ye.Kind != KindBotCheck || !strings.Contains(ye.Detail, "may have expired") {
		t.Errorf("with cookies: %v", err)
	}
	without := Downloader{Runner: &fakeRunner{stderr: botCheck, err: exitErr}, Path: "yt-dlp"}
	_, err = without.Fetch(context.Background(), vid, "d")
	if errors.As(err, &ye); strings.Contains(ye.Detail, "expired") {
		t.Errorf("hint without cookies configured: %v", err)
	}
	if strings.Contains(ye.UserMessage(), "cookie") {
		t.Error("users must not be told about cookies")
	}
}
