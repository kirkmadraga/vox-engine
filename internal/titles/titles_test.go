package titles

import "testing"

func TestOtherVersion(t *testing.T) {
	cases := []struct {
		title, wanted string
		other         bool
	}{
		{"Taylor Swift - Stay Beautiful", "taylor swift stay beautiful", false},
		{"Stay Beautiful (Karaoke Version)", "taylor swift stay beautiful", true},
		{"Stay Beautiful - Live from Clear Channel", "taylor swift stay beautiful", true},
		{"Stay Beautiful (Live)", "stay beautiful live", false}, // asked for live
		{"stay beautiful sped up", "stay beautiful", true},
		{"Stay Beautiful (Piano Cover)", "stay beautiful", true},
		{"Stay Beautiful - Remix", "Stay Beautiful (Remix)", false}, // Spotify title is a remix
		{"Uptown Funk (Discover)", "uptown funk", false},            // "cover" inside a word
		{"Delivery", "delivery", false},                             // "live" inside a word
		{"Song 8D Audio", "song", true},
		{"Song (Acapella)", "song", true},
		{"Song BACKING  TRACK", "song", true},
	}
	for _, c := range cases {
		if got := OtherVersion(c.title, c.wanted); got != c.other {
			t.Errorf("OtherVersion(%q, %q) = %v, want %v", c.title, c.wanted, got, c.other)
		}
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("  Stay\tBEAUTIFUL \n "); got != "stay beautiful" {
		t.Errorf("Normalize = %q", got)
	}
}
