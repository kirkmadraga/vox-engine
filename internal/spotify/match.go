package spotify

import (
	"regexp"
	"strings"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// ExactTitleTop is how many YouTube Music results ExactTitle looks at.
const ExactTitleTop = 3

// ExactTitle returns the first of the top ExactTitleTop YouTube Music song
// results whose title equals the track's (ignoring case and spacing). The
// search query includes the artists, so an exact title near the top is the
// official recording.
func ExactTitle(t Track, hits []ytdlp.Hit) (ytdlp.Hit, bool) {
	want := normalize(t.Title)
	for i, h := range hits {
		if i == ExactTitleTop {
			break
		}
		if want != "" && normalize(h.Title) == want {
			return h, true
		}
	}
	return ytdlp.Hit{}, false
}

// MinScore is the score a regular YouTube result needs for Best to accept it.
const MinScore = 5

// Length tolerances for Best.
const (
	closeLength = 5 * time.Second  // strong signal
	maxLength   = 15 * time.Second // beyond this, a result is ruled out
)

// rejectWords mark a different recording than the Spotify track, unless the
// Spotify title itself contains them.
var rejectWords = []string{
	"cover", "karaoke", "instrumental", "backing track", "remix", "live",
	"sped up", "slowed", "nightcore", "8d", "reverb", "acapella", "a cappella",
}

// Best picks the regular YouTube result most likely to be the track, or
// reports false if none is a confident match.
func Best(t Track, hits []ytdlp.Hit) (ytdlp.Hit, bool) {
	best, bestScore := ytdlp.Hit{}, 0
	for _, h := range hits {
		if s, ok := Score(t, h); ok && s > bestScore {
			best, bestScore = h, s
		}
	}
	return best, bestScore >= MinScore
}

// Score rates how well h matches t; ok is false if h is ruled out.
func Score(t Track, h ytdlp.Hit) (score int, ok bool) {
	// Length: needs both lengths.
	if t.Duration <= 0 || h.Duration <= 0 {
		return 0, false
	}
	switch diff := (h.Duration - t.Duration).Abs(); {
	case diff <= closeLength:
		score += 3
	case diff <= maxLength:
		score++
	default:
		return 0, false
	}

	title, spotifyTitle := normalize(h.Title), normalize(t.Title)
	for _, w := range rejectWords {
		if hasWord(title, w) && !hasWord(spotifyTitle, w) {
			return 0, false
		}
	}
	if core := coreTitle(t.Title); core != "" && strings.Contains(title, core) {
		score += 2
	}

	channel := normalize(h.Channel)
	for _, a := range t.ArtistList() {
		a = normalize(a)
		if a != "" && strings.Contains(channel, a) { // also matches "Artist - Topic"
			score += 2
			break
		}
	}
	if h.Verified {
		score++
	}
	return score, true
}

// normalize lowercases and collapses whitespace.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

var bracketed = regexp.MustCompile(`\s*[(\[][^)\]]*[)\]]`)

// coreTitle drops bracketed parts ("(feat. X)", "[Remastered]") and anything
// after " - " ("Song - Radio Edit"), then normalizes.
func coreTitle(s string) string {
	s = bracketed.ReplaceAllString(s, "")
	s, _, _ = strings.Cut(s, " - ")
	return normalize(s)
}

// hasWord reports whether phrase w appears in s as whole words.
func hasWord(s, w string) bool {
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
