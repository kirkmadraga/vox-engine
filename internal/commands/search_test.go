package commands

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// fakeSearch records queries and returns fixed results.
type fakeSearch struct {
	hits    []ytdlp.Hit
	err     error
	queries []string
	n       int
}

func (f *fakeSearch) SearchN(_ context.Context, q string, n int) ([]ytdlp.Hit, error) {
	f.queries, f.n = append(f.queries, q), n
	return f.hits, f.err
}

// searchPlay is play with search enabled, for user 5 in voice channel 77.
func searchPlay(q *fakeQueue, s *fakeSearch, recent *RecentSearches) Play {
	return Play{Voice: fakeLocator{5: 77}, Queue: q, YouTube: loaderFor, Search: s.SearchN, Results: 7, Recent: recent}
}

// fakeClock is a settable time source.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

var threeHits = []ytdlp.Hit{
	{ID: "dQw4w9WgXcQ", Title: "Never Gonna Give You Up", Duration: 213 * time.Second},
	{ID: "jNQXAC9IVRw", Title: "Me_at *the* zoo"},
	{ID: "abcdefghijk"},
}

func TestPlayWordsListResultsWithoutQueueing(t *testing.T) {
	s, q := &fakeSearch{hits: threeHits}, &fakeQueue{}
	recent := NewRecentSearches(0, nil)
	r := runCmd(t, searchPlay(q, s, recent), "never gonna")
	want := "1. **Never Gonna Give You Up** (3:33) <https://youtu.be/dQw4w9WgXcQ>\n" +
		`2. **Me\_at \*the\* zoo** <https://youtu.be/jNQXAC9IVRw>` + "\n" +
		"3. <https://youtu.be/abcdefghijk>\n" +
		"To pick one, play its number."
	if r.Content != want {
		t.Errorf("reply:\n%s\nwant:\n%s", r.Content, want)
	}
	if len(s.queries) != 1 || s.queries[0] != "never gonna" || s.n != 7 {
		t.Errorf("searched %q for %d results", s.queries, s.n)
	}
	if len(q.queued) != 0 {
		t.Errorf("search words must not queue anything: %+v", q.queued)
	}
	if got, ok := recent.Get(1, 5); !ok || len(got) != 3 {
		t.Errorf("remembered %+v, %v", got, ok)
	}
}

// Listing needs no voice channel; only picking does.
func TestPlayWordsListWithoutVoice(t *testing.T) {
	s := &fakeSearch{hits: threeHits}
	p := Play{Voice: fakeLocator{}, Queue: &fakeQueue{}, YouTube: loaderFor, Search: s.SearchN, Recent: NewRecentSearches(0, nil)}
	if r := runCmd(t, p, "never gonna"); !strings.HasPrefix(r.Content, "1. ") {
		t.Errorf("reply = %q", r.Content)
	}
	if r := runCmd(t, p, "1"); !strings.Contains(r.Content, "Join a voice channel first") {
		t.Errorf("pick without voice: reply = %q", r.Content)
	}
}

func TestPlaySearchFailures(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		wantErr bool
	}{
		{"no results", ytdlp.ErrNoResults, "No YouTube results for that.", false},
		{"bot check", &ytdlp.Error{Kind: ytdlp.KindBotCheck}, "Couldn't search: YouTube is blocking", true},
		{"missing tool", &ytdlp.Error{Kind: ytdlp.KindMissingTool}, "Couldn't search: The bot can't download", true},
		{"other", &ytdlp.Error{Kind: ytdlp.KindTimeout}, "The YouTube search failed.", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, rep, recent := &fakeQueue{}, &fakeReplier{}, NewRecentSearches(0, nil)
			err := searchPlay(q, &fakeSearch{err: c.err}, recent).
				Run(context.Background(), Request{GuildID: 1, ChannelID: 9, AuthorID: 5, Args: "some song", Reply: rep})
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if len(rep.got) != 1 || !strings.Contains(rep.got[0].Content, c.want) {
				t.Errorf("replies = %+v, want containing %q", rep.got, c.want)
			}
			if _, ok := recent.Get(1, 5); ok || len(q.queued) != 0 {
				t.Errorf("a failed search must not be remembered or queue anything: %+v", q.queued)
			}
		})
	}
}

func TestPlayWithoutSearchRejectsWords(t *testing.T) {
	for _, p := range []Play{
		{Voice: fakeLocator{5: 77}, Queue: &fakeQueue{}, YouTube: loaderFor},
		{Voice: fakeLocator{5: 77}, Queue: &fakeQueue{}, YouTube: loaderFor, Search: (&fakeSearch{}).SearchN},
	} {
		if r := runCmd(t, p, "some song"); !strings.Contains(r.Content, "doesn't look like a YouTube video link") {
			t.Errorf("reply = %q", r.Content)
		}
	}
}

func TestPlayNumberPicksFromLastSearch(t *testing.T) {
	recent := NewRecentSearches(0, nil)
	recent.Put(1, 5, threeHits)
	s, q := &fakeSearch{}, &fakeQueue{}
	r := runCmd(t, searchPlay(q, s, recent), "2")
	if r.Content != `Getting **Me\_at \*the\* zoo** ready…` {
		t.Errorf("reply = %q", r.Content)
	}
	if len(s.queries) != 0 || len(q.queued) != 1 {
		t.Fatalf("searched %v, queued %+v", s.queries, q.queued)
	}
	tr := q.queued[0]
	if tr.Key != "jNQXAC9IVRw" || tr.URL != "https://youtu.be/jNQXAC9IVRw" || tr.VoiceChannel != 77 || tr.RequestedBy != 5 {
		t.Errorf("track = %+v", tr)
	}
	if l, _ := tr.Load(context.Background()); l.Path != "jNQXAC9IVRw" {
		t.Errorf("track loads %q, want the picked video id", l.Path)
	}
	// The list stays, so another result can be picked too.
	runCmd(t, searchPlay(q, s, recent), "1")
	if len(q.queued) != 2 || q.queued[1].Key != "dQw4w9WgXcQ" || q.queued[1].Duration != 213*time.Second {
		t.Errorf("queued %+v", q.queued)
	}
}

func TestPlayNumberOutOfRange(t *testing.T) {
	recent := NewRecentSearches(0, nil)
	recent.Put(1, 5, threeHits)
	for _, n := range []string{"0", "4", "999"} {
		s, q := &fakeSearch{}, &fakeQueue{}
		r := runCmd(t, searchPlay(q, s, recent), n)
		if r.Content != "Pick a number from 1 to 3." || len(q.queued) != 0 || len(s.queries) != 0 {
			t.Errorf("%s: reply %q, queued %+v, searched %v", n, r.Content, q.queued, s.queries)
		}
	}
}

func TestPlayNumberNeedsOwnRecentSearch(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	recent := NewRecentSearches(5*time.Minute, clk.Now)
	recent.Put(1, 6, threeHits) // another user, same server
	recent.Put(2, 5, threeHits) // same user, another server
	s, q := &fakeSearch{}, &fakeQueue{}
	if r := runCmd(t, searchPlay(q, s, recent), "1"); !strings.Contains(r.Content, "No recent search") {
		t.Errorf("reply = %q", r.Content)
	}

	recent.Put(1, 5, threeHits)
	clk.t = clk.t.Add(5 * time.Minute) // expired
	if r := runCmd(t, searchPlay(q, s, recent), "1"); !strings.Contains(r.Content, "No recent search") {
		t.Errorf("expired: reply = %q", r.Content)
	}
	if r := runCmd(t, Play{Voice: fakeLocator{5: 77}, Queue: q, YouTube: loaderFor}, "1"); !strings.Contains(r.Content, "No recent search") {
		t.Errorf("no store: reply = %q", r.Content)
	}
	if len(q.queued) != 0 || len(s.queries) != 0 {
		t.Errorf("queued %+v, searched %v", q.queued, s.queries)
	}
}

func TestParsePick(t *testing.T) {
	for in, want := range map[string]int{"1": 1, "05": 5, "999": 999} {
		if n, ok := parsePick(in); !ok || n != want {
			t.Errorf("parsePick(%q) = %d, %v", in, n, ok)
		}
	}
	for _, in := range []string{"", "1000", "+2", "-1", "2 songs", "٣", "1.5"} {
		if _, ok := parsePick(in); ok {
			t.Errorf("parsePick(%q) accepted", in)
		}
	}
}

func TestRecentSearchesPrunesExpired(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	recent := NewRecentSearches(time.Minute, clk.Now)
	recent.Put(1, 5, threeHits)
	clk.t = clk.t.Add(2 * time.Minute)
	recent.Put(1, 6, threeHits)
	if len(recent.m) != 1 {
		t.Errorf("entries = %d, want expired ones pruned", len(recent.m))
	}
}

// The ytdlp searcher satisfies the function type play uses.
var _ = Play{Search: (&ytdlp.Searcher{}).SearchN}

func TestPlayEmptyResultsAreNoResults(t *testing.T) {
	recent := NewRecentSearches(0, nil)
	r := runCmd(t, searchPlay(&fakeQueue{}, &fakeSearch{hits: []ytdlp.Hit{}}, recent), "zzz")
	if r.Content != "No YouTube results for that." {
		t.Errorf("reply = %q", r.Content)
	}
	if _, ok := recent.Get(1, 5); ok {
		t.Error("an empty result list must not be remembered")
	}
}

func TestPlayNewSearchReplacesList(t *testing.T) {
	recent, q := NewRecentSearches(0, nil), &fakeQueue{}
	runCmd(t, searchPlay(q, &fakeSearch{hits: threeHits}, recent), "first search")
	runCmd(t, searchPlay(q, &fakeSearch{hits: []ytdlp.Hit{{ID: "zzzzzzzzzzz", Title: "Second"}}}, recent), "second search")
	runCmd(t, searchPlay(q, &fakeSearch{}, recent), "1")
	if len(q.queued) != 1 || q.queued[0].Key != "zzzzzzzzzzz" {
		t.Errorf("queued %+v, want the newer search's first result", q.queued)
	}
	if r := runCmd(t, searchPlay(q, &fakeSearch{}, recent), "2"); r.Content != "Pick a number from 1 to 1." {
		t.Errorf("reply = %q", r.Content)
	}
}

// Only a bare number picks; a number with words is a search.
func TestPlayNumberWithWordsSearches(t *testing.T) {
	recent := NewRecentSearches(0, nil)
	recent.Put(1, 5, threeHits)
	s, q := &fakeSearch{hits: threeHits}, &fakeQueue{}
	runCmd(t, searchPlay(q, s, recent), "1999 prince")
	if len(s.queries) != 1 || s.queries[0] != "1999 prince" || len(q.queued) != 0 {
		t.Errorf("searched %q, queued %+v", s.queries, q.queued)
	}
}

func TestPlayNumberJustBeforeExpiry(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	recent := NewRecentSearches(5*time.Minute, clk.Now)
	recent.Put(1, 5, threeHits)
	clk.t = clk.t.Add(5*time.Minute - time.Second)
	q := &fakeQueue{}
	runCmd(t, searchPlay(q, &fakeSearch{}, recent), "3")
	if len(q.queued) != 1 || q.queued[0].Key != "abcdefghijk" {
		t.Errorf("queued %+v", q.queued)
	}
}

func TestRecentSearchesConcurrentUse(t *testing.T) {
	recent := NewRecentSearches(0, nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				user := snowflake.ID(i)
				recent.Put(1, user, threeHits[:1+j%3])
				if hits, ok := recent.Get(1, user); !ok || len(hits) != 1+j%3 {
					t.Errorf("user %d: got %d hits, %v", i, len(hits), ok)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Ten results with YouTube's longest titles (100 characters) still fit, footer included.
func TestTenLongResultsFitOneMessage(t *testing.T) {
	var hits []ytdlp.Hit
	for i := range 10 {
		hits = append(hits, ytdlp.Hit{ID: "abcdefghij" + string(rune('a'+i)), Title: strings.Repeat("Title_", 17)[:100], Duration: 3*time.Hour + 59*time.Minute})
	}
	out := formatResults(hits)
	if !strings.HasSuffix(out, "To pick one, play its number.") || !strings.Contains(out, "10. ") {
		t.Errorf("list was cut short (%d chars):\n%s", len(out), out)
	}
}
