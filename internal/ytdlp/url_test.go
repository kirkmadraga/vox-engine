package ytdlp

import (
	"errors"
	"testing"
)

func TestParseVideoID(t *testing.T) {
	const id = "jNQXAC9IVRw"
	ok := []string{
		"https://www.youtube.com/watch?v=jNQXAC9IVRw",
		"https://youtube.com/watch?v=jNQXAC9IVRw",
		"http://www.youtube.com/watch?v=jNQXAC9IVRw",
		"https://m.youtube.com/watch?v=jNQXAC9IVRw",
		"https://music.youtube.com/watch?v=jNQXAC9IVRw&si=abc",
		"https://www.youtube.com/watch?v=jNQXAC9IVRw&t=42s&feature=share",
		"https://www.youtube.com/watch?v=jNQXAC9IVRw&list=PL1234567890", // video inside a playlist: just the video
		"https://www.youtube.com/watch?feature=share&v=jNQXAC9IVRw",
		"https://youtu.be/jNQXAC9IVRw",
		"https://youtu.be/jNQXAC9IVRw?si=xyz&t=10",
		"https://www.youtube.com/shorts/jNQXAC9IVRw",
		"https://youtube.com/shorts/jNQXAC9IVRw?feature=share",
		"youtu.be/jNQXAC9IVRw",
		"www.youtube.com/watch?v=jNQXAC9IVRw",
		"<https://youtu.be/jNQXAC9IVRw>",
		"  https://YOUTU.BE/jNQXAC9IVRw  ",
	}
	for _, in := range ok {
		got, err := ParseVideoID(in)
		if err != nil || got != id {
			t.Errorf("ParseVideoID(%q) = %q, %v; want %q", in, got, err, id)
		}
	}

	bad := map[string]error{
		"":                                     ErrNotYouTube,
		"hello":                                ErrNotYouTube,
		"https://vimeo.com/123456":             ErrNotYouTube,
		"https://evil.com/watch?v=jNQXAC9IVRw": ErrNotYouTube,
		"https://youtube.com.evil.com/watch?v=jNQXAC9IVRw":   ErrNotYouTube,
		"https://www.youtube.com/watch":                      ErrNotYouTube,
		"https://www.youtube.com/watch?v=short":              ErrNotYouTube,
		"https://www.youtube.com/watch?v=jNQXAC9IVRwX":       ErrNotYouTube, // 12 chars
		"https://youtu.be/../../etc/passwd":                  ErrNotYouTube,
		"https://www.youtube.com/channel/UCabc":              ErrNotYouTube,
		"https://www.youtube.com/@someone":                   ErrNotYouTube,
		"ftp://youtu.be/jNQXAC9IVRw":                         ErrNotYouTube,
		"https://www.youtube.com/playlist?list=PL1234567890": ErrPlaylist,
	}
	for in, want := range bad {
		if got, err := ParseVideoID(in); !errors.Is(err, want) {
			t.Errorf("ParseVideoID(%q) = %q, %v; want %v", in, got, err, want)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"jNQXAC9IVRw": true,
		"-abc_DEF123": true,
		"abc":         false,
		"../../x/y/z": false,
		"abc def 123": false,
	} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v", id, !want)
		}
	}
}
