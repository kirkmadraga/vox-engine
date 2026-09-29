//go:build integration

// Live test: real Spotify pages, real yt-dlp searches. Run with:
//
//	go test -tags integration ./internal/spotify/
//
// Optional environment: YTDLP_PATH (default: yt-dlp on PATH).
package spotify

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

func liveSearcher(t *testing.T) *ytdlp.Searcher {
	t.Helper()
	path := os.Getenv("YTDLP_PATH")
	if path == "" {
		var err error
		if path, err = exec.LookPath("yt-dlp"); err != nil {
			t.Skip("yt-dlp not found")
		}
	}
	return &ytdlp.Searcher{Runner: ytdlp.ExecRunner{}, Path: path}
}

func TestLiveTrackPages(t *testing.T) {
	c := &Client{}
	cases := map[string]Track{
		"4PTG3Z6ehGkBFwjybzWkR8": {Title: "Never Gonna Give You Up", Artists: "Rick Astley", Duration: 214 * time.Second},
		"32OlwWuMpZ6b0aN2RZOeMS": {Title: "Uptown Funk (feat. Bruno Mars)", Artists: "Mark Ronson, Bruno Mars", Duration: 270 * time.Second},
	}
	for id, want := range cases {
		got, err := c.Track(context.Background(), id)
		want.ID = id
		if err != nil || got != want {
			t.Errorf("Track(%s) = %+v, %v; want %+v", id, got, err, want)
		}
	}
}

// The whole match: YouTube Music first, then scored regular search.
func TestLiveMatch(t *testing.T) {
	s := liveSearcher(t)
	for _, id := range []string{"4PTG3Z6ehGkBFwjybzWkR8", "32OlwWuMpZ6b0aN2RZOeMS"} {
		track, err := (&Client{}).Track(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		query := track.Artists + " " + track.Title
		music, err := s.SearchMusic(context.Background(), query, ExactTitleTop)
		if h, ok := ExactTitle(track, music); err == nil && ok {
			t.Logf("%s: YouTube Music %s %q", track.Name(), h.ID, h.Title)
			continue
		}
		hits, err := s.SearchN(context.Background(), query, 5)
		if err != nil {
			t.Fatal(err)
		}
		h, ok := Best(track, hits)
		if !ok {
			t.Errorf("%s: no confident match in %+v", track.Name(), hits)
			continue
		}
		t.Logf("%s: YouTube %s %q (%s)", track.Name(), h.ID, h.Title, h.Channel)
	}
}
