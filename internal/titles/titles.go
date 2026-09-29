// Package titles compares song and video titles: normalizing them and
// spotting a different version of a song (a cover, karaoke, a live take...).
package titles

import "strings"

// otherVersionWords mark a different recording than the one wanted.
var otherVersionWords = []string{
	"cover", "karaoke", "instrumental", "backing track", "remix", "live",
	"sped up", "slowed", "nightcore", "8d", "reverb", "acapella", "a cappella",
}

// OtherVersion reports whether title looks like a different version (cover,
// karaoke, live, sped up...) than wanted describes. A word counts only if
// wanted doesn't contain it too, so asking for "song live" accepts a live take.
func OtherVersion(title, wanted string) bool {
	title, wanted = Normalize(title), Normalize(wanted)
	for _, w := range otherVersionWords {
		if HasWord(title, w) && !HasWord(wanted, w) {
			return true
		}
	}
	return false
}

// Normalize lowercases and collapses whitespace.
func Normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// HasWord reports whether phrase w appears in s as whole words (s and w
// already normalized).
func HasWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(w)
		if (start == 0 || !isWordByte(s[start-1])) && (end == len(s) || !isWordByte(s[end])) {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}
