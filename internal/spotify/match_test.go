package spotify

import (
	"testing"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

var uptown = Track{Title: "Uptown Funk (feat. Bruno Mars)", Artists: "Mark Ronson, Bruno Mars", Duration: 270 * time.Second}

func TestExactTitle(t *testing.T) {
	hits := []ytdlp.Hit{
		{ID: "a", Title: "Uptown Funk!"},
		{ID: "b", Title: "  uptown FUNK   (feat. Bruno Mars) "},
		{ID: "c", Title: "Uptown Funk (feat. Bruno Mars)"},
	}
	if h, ok := ExactTitle(uptown, hits); !ok || h.ID != "b" {
		t.Errorf("ExactTitle = %+v, %v; want the first exact title (case and spacing ignored)", h, ok)
	}
	// Close isn't exact.
	if _, ok := ExactTitle(uptown, hits[:1]); ok {
		t.Error(`"Uptown Funk!" must not count as exact`)
	}
	// Only the top ExactTitleTop results count.
	late := []ytdlp.Hit{{Title: "x"}, {Title: "y"}, {Title: "z"}, {ID: "d", Title: uptown.Title}}
	if _, ok := ExactTitle(uptown, late); ok {
		t.Error("an exact title below the top 3 must not count")
	}
	if _, ok := ExactTitle(Track{}, []ytdlp.Hit{{ID: "e"}}); ok {
		t.Error("an empty title must never match")
	}
}

// Real regular-YouTube results for "Mark Ronson, Bruno Mars Uptown Funk" (2026-09-30).
var uptownResults = []ytdlp.Hit{
	{ID: "PjQZJXq-yfw", Duration: 194 * time.Second, Channel: "Enrico Meloni", Title: "Bruno Mars - Uptown Funk (Cover by The GrooveFellas, Wedding Band Italy and Tuscany)"},
	{ID: "yapD_gy-HGQ", Duration: 270 * time.Second, Channel: "All About The Music", Title: "Mark Ronson ft. Bruno Mars - Uptown Funk Bassless (Bass Backing Track - Not A Cover)"},
	{ID: "W8FUmkw3a4U", Duration: 269 * time.Second, Channel: "7clouds", Verified: true, Title: "Mark Ronson - Uptown Funk (Lyrics) ft. Bruno Mars"},
	{ID: "OPf0YbXqDm0", Duration: 271 * time.Second, Channel: "Mark Ronson", Verified: true, Title: "Mark Ronson - Uptown Funk (Official Video) ft. Bruno Mars"},
	{ID: "5dkATLgthdI", Duration: 190 * time.Second, Channel: "MUSIGLOBE", Title: "Mark Ronson - Uptown Funk (Official Video) ft. Bruno Mars | MUSIGLOBE"},
}

func TestBestPicksOfficialOverBackingTrack(t *testing.T) {
	h, ok := Best(uptown, uptownResults)
	if !ok || h.ID != "OPf0YbXqDm0" {
		t.Errorf("Best = %+v, %v; want the official video", h, ok)
	}
	if _, ok := Score(uptown, uptownResults[1]); ok {
		t.Error("the backing track (exact length!) must be ruled out")
	}
}

func TestScoreRules(t *testing.T) {
	base := ytdlp.Hit{Duration: 270 * time.Second, Title: "Uptown Funk"}
	cases := []struct {
		name  string
		h     ytdlp.Hit
		t     Track
		score int
		ok    bool
	}{
		{"close length + title", base, uptown, 3 + 2, true},
		{"length within 15 s", ytdlp.Hit{Duration: 280 * time.Second, Title: "Uptown Funk"}, uptown, 1 + 2, true},
		{"length too far", ytdlp.Hit{Duration: 300 * time.Second, Title: "Uptown Funk"}, uptown, 0, false},
		{"unknown length", ytdlp.Hit{Title: "Uptown Funk"}, uptown, 0, false},
		{"no Spotify length", base, Track{Title: "Uptown Funk"}, 0, false},
		{"artist channel", ytdlp.Hit{Duration: 270 * time.Second, Title: "x", Channel: "Bruno Mars"}, uptown, 3 + 2, true},
		{"topic channel", ytdlp.Hit{Duration: 270 * time.Second, Title: "x", Channel: "Mark Ronson - Topic"}, uptown, 3 + 2, true},
		{"verified", ytdlp.Hit{Duration: 270 * time.Second, Title: "x", Verified: true}, uptown, 3 + 1, true},
		{"cover", ytdlp.Hit{Duration: 270 * time.Second, Title: "Uptown Funk (Cover)"}, uptown, 0, false},
		{"karaoke", ytdlp.Hit{Duration: 270 * time.Second, Title: "Uptown Funk Karaoke"}, uptown, 0, false},
		{"live", ytdlp.Hit{Duration: 270 * time.Second, Title: "Uptown Funk - Live at X"}, uptown, 0, false},
		{"sped up", ytdlp.Hit{Duration: 270 * time.Second, Title: "uptown funk sped up"}, uptown, 0, false},
		{"reject word inside another word", ytdlp.Hit{Duration: 270 * time.Second, Title: "Uptown Funk (Discover)"}, uptown, 3 + 2, true},
		{"live allowed if Spotify says live", ytdlp.Hit{Duration: 330 * time.Second, Title: "Vincent (Live in Austin)"},
			Track{Title: "Vincent - Live in Austin", Duration: 330 * time.Second}, 3 + 2, true},
		{"remix allowed if Spotify says remix", ytdlp.Hit{Duration: 200 * time.Second, Title: "Song (Remix)"},
			Track{Title: "Song (Remix)", Duration: 200 * time.Second}, 3 + 2, true},
	}
	for _, c := range cases {
		score, ok := Score(c.t, c.h)
		if score != c.score || ok != c.ok {
			t.Errorf("%s: Score = %d, %v; want %d, %v", c.name, score, ok, c.score, c.ok)
		}
	}
}

func TestBestNeedsMinScore(t *testing.T) {
	// Length within 15 s and verified, but nothing else: 1 + 1 < MinScore.
	weak := []ytdlp.Hit{{ID: "w", Duration: 280 * time.Second, Title: "something else", Verified: true}}
	if h, ok := Best(uptown, weak); ok {
		t.Errorf("Best accepted a weak match: %+v", h)
	}
	if _, ok := Best(uptown, nil); ok {
		t.Error("no results must not match")
	}
}

func TestCoreTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Uptown Funk (feat. Bruno Mars)":      "uptown funk",
		"Bohemian Rhapsody - Remastered 2011": "bohemian rhapsody",
		"Song [Official] (Live)":              "song",
		"  Plain  ":                           "plain",
	} {
		if got := coreTitle(in); got != want {
			t.Errorf("coreTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
