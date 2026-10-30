package mediafile

import "testing"

func TestCheckLink(t *testing.T) {
	for raw, want := range map[string]string{
		"https://archive.org/download/x/a.mp3":   "https://archive.org/download/x/a.mp3",
		" <https://cdn.discordapp.com/a.ogg?x> ": "https://cdn.discordapp.com/a.ogg?x",
		"example.com/a.mp3":                      "https://example.com/a.mp3",
		"http://8.8.8.8/a.mp3":                   "http://8.8.8.8/a.mp3",
	} {
		if got, err := CheckLink(raw); err != nil || got != want {
			t.Errorf("CheckLink(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for raw, want := range map[string]Kind{
		"ftp://example.com/a.mp3":       KindBadLink,
		"file:///etc/passwd":            KindBadLink,
		"https://":                      KindBadLink,
		"http://localhost/a.mp3":        KindBlocked,
		"http://LOCALHOST./a.mp3":       KindBlocked,
		"http://api.localhost/a.mp3":    KindBlocked,
		"http://127.0.0.1:9000/a.mp3":   KindBlocked,
		"http://10.0.0.5/a.mp3":         KindBlocked,
		"http://[::1]/a.mp3":            KindBlocked,
		"http://[::ffff:127.0.0.1]/a":   KindBlocked,
		"http://169.254.169.254/latest": KindBlocked,
	} {
		if _, err := CheckLink(raw); kindOf(err) != want {
			t.Errorf("CheckLink(%q): err %v, want %v", raw, err, want)
		}
	}
}

func TestIsMedia(t *testing.T) {
	for _, c := range []struct {
		name, ct string
		want     bool
	}{
		{"a.mp3", "audio/mpeg", true},
		{"voice-message.ogg", "audio/ogg", true},
		{"clip.mov", "video/quicktime", true},
		{"x", "Audio/Wav; codecs=1", true},
		{"a.ogg", "application/ogg", true},
		{"a.FLAC", "", true},
		{"a.mkv", "application/octet-stream", true},
		{"a.txt", "", false},
		{"noext", "", false},
		{"a.mp3", "text/plain", false}, // the type wins over the name
		{"a.zip", "application/zip", false},
		{"a.png", "image/png", false},
	} {
		if got := IsMedia(c.name, c.ct); got != c.want {
			t.Errorf("IsMedia(%q, %q) = %v", c.name, c.ct, got)
		}
	}
}
