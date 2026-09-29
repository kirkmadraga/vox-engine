package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/queue"
)

type fakeLocator map[snowflake.ID]snowflake.ID // user -> voice channel

func (f fakeLocator) UserVoiceChannel(_, user snowflake.ID) (snowflake.ID, bool) {
	ch, ok := f[user]
	return ch, ok
}

type fakeQueue struct {
	pos      queue.Position
	err      error
	queued   []queue.Track
	listing  queue.Listing
	skipped  *queue.Track
	stopped  bool
	guildArg snowflake.ID
}

func (f *fakeQueue) Enqueue(g snowflake.ID, t queue.Track) (queue.Position, error) {
	f.guildArg = g
	if f.err != nil {
		return queue.Position{}, f.err
	}
	f.queued = append(f.queued, t)
	return f.pos, nil
}
func (f *fakeQueue) Skip(snowflake.ID) (queue.Track, bool) {
	if f.skipped == nil {
		return queue.Track{}, false
	}
	return *f.skipped, true
}
func (f *fakeQueue) Stop(snowflake.ID) bool          { return f.stopped }
func (f *fakeQueue) List(snowflake.ID) queue.Listing { return f.listing }

func loaderFor(id string) queue.LoadFunc {
	return func(context.Context) (queue.Loaded, error) { return queue.Loaded{Path: id}, nil }
}

// runCmd runs cmd as user 5 (in voice channel 77) in guild 1, text channel 9.
func runCmd(t *testing.T, cmd Command, args string) Reply {
	t.Helper()
	rep := &fakeReplier{}
	if err := cmd.Run(context.Background(), Request{GuildID: 1, ChannelID: 9, AuthorID: 5, Args: args, Reply: rep}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.got) != 1 {
		t.Fatalf("got %d replies, want 1", len(rep.got))
	}
	if len(rep.got[0].Mentions) != 0 {
		t.Errorf("playback replies must not ping: %v", rep.got[0].Mentions)
	}
	return rep.got[0]
}

func TestPlayQueuesVideo(t *testing.T) {
	q := &fakeQueue{}
	p := Play{Voice: fakeLocator{5: 77}, Queue: q, YouTube: loaderFor}
	r := runCmd(t, p, "https://www.youtube.com/watch?v=jNQXAC9IVRw&t=5")
	if r.Content != "Getting <https://youtu.be/jNQXAC9IVRw> ready…" {
		t.Errorf("reply = %q", r.Content)
	}
	if len(q.queued) != 1 || q.guildArg != 1 {
		t.Fatalf("queued = %+v", q.queued)
	}
	tr := q.queued[0]
	if tr.Key != "jNQXAC9IVRw" || tr.URL != "https://youtu.be/jNQXAC9IVRw" || tr.RequestedBy != 5 || tr.VoiceChannel != 77 || tr.TextChannel != 9 {
		t.Errorf("track = %+v", tr)
	}
	if l, _ := tr.Load(context.Background()); l.Path != "jNQXAC9IVRw" {
		t.Errorf("track loads %q, want the parsed video id", l.Path)
	}
}

func TestPlayReplies(t *testing.T) {
	cases := []struct {
		name string
		q    *fakeQueue
		args string
		want string
	}{
		{"no link", &fakeQueue{}, "", "Usage: `play <YouTube link>`"},
		{"not youtube", &fakeQueue{}, "https://vimeo.com/1", "That doesn't look like a YouTube video link."},
		{"playlist", &fakeQueue{}, "https://www.youtube.com/playlist?list=PLx", "Playlists aren't supported"},
		{"queued behind", &fakeQueue{pos: queue.Position{Ahead: 2}}, "youtu.be/jNQXAC9IVRw", "Queued <https://youtu.be/jNQXAC9IVRw> (2 ahead of it)."},
		{"other server", &fakeQueue{pos: queue.Position{OtherGuild: true}}, "youtu.be/jNQXAC9IVRw", "playing in another server"},
		{"full", &fakeQueue{err: queue.ErrFull}, "youtu.be/jNQXAC9IVRw", "The queue is full."},
		{"other error", &fakeQueue{err: errors.New("shutting down")}, "youtu.be/jNQXAC9IVRw", "Couldn't queue that"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runCmd(t, Play{Voice: fakeLocator{5: 77}, Queue: c.q, YouTube: loaderFor}, c.args)
			if !strings.Contains(r.Content, c.want) {
				t.Errorf("reply = %q, want containing %q", r.Content, c.want)
			}
		})
	}
}

func TestPlayAndTestNeedVoice(t *testing.T) {
	q := &fakeQueue{}
	for _, cmd := range []Command{
		Play{Voice: fakeLocator{}, Queue: q, YouTube: loaderFor},
		Test{Voice: fakeLocator{}, Queue: q, Tone: loaderFor("tone")},
	} {
		if r := runCmd(t, cmd, "youtu.be/jNQXAC9IVRw"); !strings.Contains(r.Content, "Join a voice channel first") {
			t.Errorf("%s: reply = %q", cmd.Name(), r.Content)
		}
	}
	if len(q.queued) != 0 {
		t.Error("nothing should be queued when the caller is not in voice")
	}
}

func TestTestQueuesTone(t *testing.T) {
	q := &fakeQueue{}
	r := runCmd(t, Test{Voice: fakeLocator{5: 77}, Queue: q, Tone: loaderFor("tone.opus")}, "")
	if r.Content != "Getting **Test tone** ready…" {
		t.Errorf("reply = %q", r.Content)
	}
	if len(q.queued) != 1 || q.queued[0].Title != "Test tone" || q.queued[0].VoiceChannel != 77 {
		t.Errorf("queued = %+v", q.queued)
	}
}

func TestQueueList(t *testing.T) {
	if r := runCmd(t, QueueList{Queue: &fakeQueue{}}, ""); r.Content != "The queue is empty." {
		t.Errorf("empty: %q", r.Content)
	}
	q := &fakeQueue{listing: queue.Listing{
		Current: &queue.Track{Title: "Me at the zoo", Duration: 19 * time.Second, RequestedBy: 5},
		Upcoming: []queue.Track{
			{URL: "https://youtu.be/abcdefghijk", RequestedBy: 6},
			{Title: "Song_2", Duration: 2*time.Minute + 1*time.Second, RequestedBy: 5},
		},
	}}
	want := "**Now playing:** **Me at the zoo** (0:19), requested by <@5>\n" +
		"**Up next:**\n" +
		"1. <https://youtu.be/abcdefghijk>, requested by <@6>\n" +
		"2. **Song\\_2** (2:01), requested by <@5>"
	if r := runCmd(t, QueueList{Queue: q}, ""); r.Content != want {
		t.Errorf("listing:\n%s\nwant:\n%s", r.Content, want)
	}
}

func TestSkipAndStop(t *testing.T) {
	if r := runCmd(t, Skip{Queue: &fakeQueue{}}, ""); r.Content != "Nothing is playing." {
		t.Errorf("skip idle: %q", r.Content)
	}
	q := &fakeQueue{skipped: &queue.Track{Title: "Me at the zoo"}, stopped: true}
	if r := runCmd(t, Skip{Queue: q}, ""); r.Content != "Skipped **Me at the zoo**." {
		t.Errorf("skip: %q", r.Content)
	}
	if r := runCmd(t, Stop{Queue: &fakeQueue{}}, ""); r.Content != "Nothing is playing." {
		t.Errorf("stop idle: %q", r.Content)
	}
	if r := runCmd(t, Stop{Queue: q}, ""); r.Content != "Stopped and cleared the queue." {
		t.Errorf("stop: %q", r.Content)
	}
}

// The real queue satisfies the interface the commands use.
var _ Queue = (*queue.Manager)(nil)
