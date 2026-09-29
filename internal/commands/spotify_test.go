package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/spotify"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

const uptownLink = "https://open.spotify.com/track/32OlwWuMpZ6b0aN2RZOeMS?si=x"

var uptownTrack = spotify.Track{ID: "32OlwWuMpZ6b0aN2RZOeMS", Title: "Uptown Funk (feat. Bruno Mars)", Artists: "Mark Ronson, Bruno Mars", Duration: 270 * time.Second}

// fakeSpotify returns a fixed track and records lookups.
type fakeSpotify struct {
	track spotify.Track
	err   error
	ids   []string
}

func (f *fakeSpotify) Track(_ context.Context, id string) (spotify.Track, error) {
	f.ids = append(f.ids, id)
	return f.track, f.err
}

type spotifySetup struct {
	sp            *fakeSpotify
	music, search *fakeSearch
	q             *fakeQueue
	recent        *RecentSearches
	logs          *bytes.Buffer
}

func newSpotifySetup() *spotifySetup {
	return &spotifySetup{
		sp:     &fakeSpotify{track: uptownTrack},
		music:  &fakeSearch{},
		search: &fakeSearch{},
		q:      &fakeQueue{},
		recent: NewRecentSearches(0, nil),
		logs:   &bytes.Buffer{},
	}
}

func (s *spotifySetup) play(voice fakeLocator) Play {
	return Play{
		Voice: voice, Queue: s.q, YouTube: loaderFor,
		Search: s.search.SearchN, Results: 10, Recent: s.recent,
		Spotify: s.sp.Track, SearchMusic: s.music.SearchN,
		Logger: slog.New(slog.NewTextHandler(s.logs, nil)),
	}
}

// logged fails unless every want appears in the log.
func (s *spotifySetup) logged(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s.logs.String(), w) {
			t.Errorf("log missing %q:\n%s", w, s.logs.String())
		}
	}
}

func (s *spotifySetup) run(t *testing.T, args string) Reply {
	t.Helper()
	return runCmd(t, s.play(fakeLocator{5: 77}), args)
}

const wantQuery = "Mark Ronson, Bruno Mars Uptown Funk (feat. Bruno Mars)"

// Step 1: an exact YouTube Music title is queued; regular search never runs.
func TestSpotifyYouTubeMusicMatch(t *testing.T) {
	s := newSpotifySetup()
	s.music.hits = []ytdlp.Hit{{ID: "RTrYmWOAdjo", Title: "Uptown Funk!"}, {ID: "tYvFa2ARD24", Title: "Uptown Funk (feat. Bruno Mars)"}}
	r := s.run(t, uptownLink)
	if r.Content != "Getting **Mark Ronson, Bruno Mars – Uptown Funk (feat. Bruno Mars)** ready…" {
		t.Errorf("reply = %q", r.Content)
	}
	if len(s.sp.ids) != 1 || s.sp.ids[0] != uptownTrack.ID {
		t.Errorf("Spotify lookups = %q", s.sp.ids)
	}
	if len(s.music.queries) != 1 || s.music.queries[0] != wantQuery || s.music.n != spotify.ExactTitleTop {
		t.Errorf("music searches = %q (n=%d)", s.music.queries, s.music.n)
	}
	if len(s.search.queries) != 0 {
		t.Errorf("regular search ran: %q", s.search.queries)
	}
	if len(s.q.queued) != 1 {
		t.Fatalf("queued = %+v", s.q.queued)
	}
	tr := s.q.queued[0]
	if tr.Key != "tYvFa2ARD24" || tr.URL != "https://youtu.be/tYvFa2ARD24" || tr.Duration != 270*time.Second || tr.VoiceChannel != 77 {
		t.Errorf("track = %+v", tr)
	}
	if tr.Title != "Mark Ronson, Bruno Mars – Uptown Funk (feat. Bruno Mars)" {
		t.Errorf("title = %q, want the Spotify name", tr.Title)
	}
	s.logged(t, "spotify: matched", "step=youtube-music", "video=tYvFa2ARD24", "spotify=32OlwWuMpZ6b0aN2RZOeMS")
}

// Step 2: no exact YouTube Music title; the best-scoring regular result is queued.
func TestSpotifyScoredMatch(t *testing.T) {
	for name, musicErr := range map[string]error{"no exact title": nil, "music search failed": errors.New("boom")} {
		t.Run(name, func(t *testing.T) {
			s := newSpotifySetup()
			s.music.hits, s.music.err = []ytdlp.Hit{{ID: "RTrYmWOAdjo", Title: "Uptown Funk!"}}, musicErr
			s.search.hits = []ytdlp.Hit{
				{ID: "yapD_gy-HGQ", Duration: 270 * time.Second, Channel: "All About The Music", Title: "Uptown Funk Bassless (Bass Backing Track)"},
				{ID: "OPf0YbXqDm0", Duration: 271 * time.Second, Channel: "Mark Ronson", Verified: true, Title: "Mark Ronson - Uptown Funk (Official Video) ft. Bruno Mars"},
			}
			s.run(t, uptownLink)
			if len(s.search.queries) != 1 || s.search.queries[0] != wantQuery || s.search.n != spotifyCandidates {
				t.Errorf("regular searches = %q (n=%d)", s.search.queries, s.search.n)
			}
			if len(s.q.queued) != 1 || s.q.queued[0].Key != "OPf0YbXqDm0" {
				t.Errorf("queued = %+v, want the official video", s.q.queued)
			}
			s.logged(t, "step=scored", "video=OPf0YbXqDm0", "score=8")
			if musicErr != nil {
				s.logged(t, "YouTube Music search failed", "err=boom")
			}
		})
	}
}

// Step 3: nothing confident; the results are listed and can be picked.
func TestSpotifyNoSureMatchListsResults(t *testing.T) {
	s := newSpotifySetup()
	s.search.hits = []ytdlp.Hit{
		{ID: "aaaaaaaaaaa", Duration: 100 * time.Second, Title: "Something else"},
		{ID: "bbbbbbbbbbb", Duration: 270 * time.Second, Title: "Uptown Funk (Karaoke)"},
	}
	r := s.run(t, uptownLink)
	if !strings.HasPrefix(r.Content, "Couldn't find a sure match for **Mark Ronson, Bruno Mars – Uptown Funk (feat. Bruno Mars)** on YouTube.\n1. ") ||
		!strings.HasSuffix(r.Content, "To pick one, play its number.") {
		t.Errorf("reply = %q", r.Content)
	}
	if len(s.q.queued) != 0 {
		t.Errorf("nothing should be queued yet: %+v", s.q.queued)
	}
	s.logged(t, "no sure match", "results=2")
	s.run(t, "2")
	if len(s.q.queued) != 1 || s.q.queued[0].Key != "bbbbbbbbbbb" {
		t.Errorf("pick queued %+v", s.q.queued)
	}
}

func TestSpotifyNeedsVoiceFirst(t *testing.T) {
	s := newSpotifySetup()
	r := runCmd(t, s.play(fakeLocator{}), uptownLink)
	if !strings.Contains(r.Content, "Join a voice channel first") || len(s.sp.ids) != 0 || len(s.music.queries)+len(s.search.queries) != 0 {
		t.Errorf("reply %q; spotify %q, music %q, search %q", r.Content, s.sp.ids, s.music.queries, s.search.queries)
	}
}

func TestSpotifyUnsupportedLinks(t *testing.T) {
	cases := map[string]string{
		"https://open.spotify.com/album/6eUW0wxWtzkFdaEFsTJto6":    "Only single Spotify tracks",
		"https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M": "Only single Spotify tracks",
		"spotify:album:6eUW0wxWtzkFdaEFsTJto6":                     "Only single Spotify tracks",
		"https://spotify.link/AbCdEf":                              "Short spotify.link links",
		"https://open.spotify.com/track/nope":                      "doesn't look like a Spotify track link",
	}
	for link, want := range cases {
		s := newSpotifySetup()
		r := s.run(t, link)
		if !strings.Contains(r.Content, want) {
			t.Errorf("%q: reply = %q, want %q", link, r.Content, want)
		}
		if len(s.sp.ids)+len(s.music.queries)+len(s.search.queries)+len(s.q.queued) != 0 {
			t.Errorf("%q: something ran", link)
		}
	}
}

func TestSpotifyFailures(t *testing.T) {
	cases := []struct {
		name      string
		spErr     error
		searchErr error
		want      string
		wantErr   bool
	}{
		{"track not found", spotify.ErrNotFound, nil, "Couldn't find that Spotify track.", false},
		{"page unreadable", errors.New("HTTP 500"), nil, "Couldn't read that Spotify link.", true},
		{"no YouTube results", nil, ytdlp.ErrNoResults, "No YouTube results for that.", false},
		{"YouTube bot check", nil, &ytdlp.Error{Kind: ytdlp.KindBotCheck}, "Couldn't search: YouTube is blocking", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSpotifySetup()
			s.sp.err, s.search.err = c.spErr, c.searchErr
			rep := &fakeReplier{}
			err := s.play(fakeLocator{5: 77}).Run(context.Background(), Request{GuildID: 1, ChannelID: 9, AuthorID: 5, Args: uptownLink, Reply: rep})
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if len(rep.got) != 1 || !strings.Contains(rep.got[0].Content, c.want) {
				t.Errorf("replies = %+v, want %q", rep.got, c.want)
			}
			if len(s.q.queued) != 0 {
				t.Errorf("queued %+v", s.q.queued)
			}
		})
	}
}

// Without Spotify support, Spotify links are rejected and never word-searched.
func TestSpotifyDisabled(t *testing.T) {
	for _, link := range []string{uptownLink, "spotify:track:32OlwWuMpZ6b0aN2RZOeMS"} {
		s := newSpotifySetup()
		p := s.play(fakeLocator{5: 77})
		p.Spotify = nil
		r := runCmd(t, p, link)
		if !strings.Contains(r.Content, "doesn't look like a YouTube video link") || len(s.search.queries) != 0 {
			t.Errorf("%q: reply %q, searched %q", link, r.Content, s.search.queries)
		}
	}
}

// YouTube links and search words never touch Spotify.
func TestNonSpotifyInputSkipsSpotify(t *testing.T) {
	s := newSpotifySetup()
	s.search.hits = threeHits
	s.run(t, "https://youtu.be/dQw4w9WgXcQ")
	s.run(t, "spotify wrapped songs")
	if len(s.sp.ids) != 0 || len(s.music.queries) != 0 {
		t.Errorf("spotify %q, music %q", s.sp.ids, s.music.queries)
	}
	if len(s.search.queries) != 1 || s.search.queries[0] != "spotify wrapped songs" {
		t.Errorf("word search = %q", s.search.queries)
	}
}
