package commands

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/mediafile"
	"github.com/kirkmadraga/vox-engine/internal/queue"
)

// fileLoads records what File was asked to load.
type fileLoads struct{ srcs []mediafile.Source }

func (f *fileLoads) load(key string, src mediafile.Source) queue.LoadFunc {
	f.srcs = append(f.srcs, src)
	return func(context.Context) (queue.Loaded, error) { return queue.Loaded{Path: key}, nil }
}

// roomQueue takes room tracks, then is full.
type roomQueue struct {
	fakeQueue
	room int
}

func (q *roomQueue) Enqueue(g snowflake.ID, t queue.Track) (queue.Position, error) {
	if len(q.queued) >= q.room {
		return queue.Position{}, queue.ErrFull
	}
	return q.fakeQueue.Enqueue(g, t)
}

func filePlay(q Queue, loads *fileLoads) Play {
	return Play{Voice: fakeLocator{5: 77}, Queue: q, YouTube: loaderFor, File: loads.load, MaxFileBytes: 1000}
}

func mp3(name string) File {
	return File{URL: "https://cdn.discordapp.com/attachments/1/2/" + name, Name: name, ContentType: "audio/mpeg", Size: 10}
}

// runReq runs cmd with req's files and quote as user 5 (in voice channel 77).
func runReq(t *testing.T, cmd Command, req Request) string {
	t.Helper()
	rep := &fakeReplier{}
	req.GuildID, req.ChannelID, req.AuthorID, req.Reply = 1, 9, 5, rep
	if err := cmd.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.got) != 1 {
		t.Fatalf("got %d replies, want 1", len(rep.got))
	}
	return rep.got[0].Content
}

func TestPlayQueuesAttachedFiles(t *testing.T) {
	q, loads := &fakeQueue{}, &fileLoads{}
	r := runReq(t, filePlay(q, loads), Request{Files: []File{
		mp3("a.mp3"),
		{URL: "u2", Name: "notes.txt", ContentType: "text/plain"}, // not media: ignored
		{URL: "u3", Name: "clip.mp4", ContentType: "video/mp4", Size: 20},
		{URL: "u4", Name: "b.flac"}, // no type: judged by name
	}})
	if r != "Getting **a.mp3**, **clip.mp4** and **b.flac** ready…" {
		t.Errorf("reply = %q", r)
	}
	if len(q.queued) != 3 || len(loads.srcs) != 3 {
		t.Fatalf("queued %+v, loads %+v", q.queued, loads.srcs)
	}
	keys := map[string]bool{}
	for i, want := range []string{"a.mp3", "clip.mp4", "b.flac"} {
		tr := q.queued[i]
		if tr.Title != want || tr.URL != "" || tr.RequestedBy != 5 || tr.VoiceChannel != 77 || tr.TextChannel != 9 {
			t.Errorf("track %d = %+v", i, tr) // attachments never show (or log) their Discord link
		}
		if !strings.HasPrefix(tr.Key, mediafile.KeyPrefix) || keys[tr.Key] {
			t.Errorf("track %d key %q: want a fresh file key", i, tr.Key)
		}
		keys[tr.Key] = true
		if l, _ := tr.Load(context.Background()); l.Path != tr.Key {
			t.Errorf("track %d loads %q, want its own key", i, l.Path)
		}
	}
	if loads.srcs[1] != (mediafile.Source{URL: "u3", Name: "clip.mp4", Size: 20}) {
		t.Errorf("source = %+v", loads.srcs[1])
	}
}

func TestPlayTakesFiveFiles(t *testing.T) {
	var files []File
	for i := range 7 {
		files = append(files, mp3(fmt.Sprintf("%d.mp3", i+1)))
	}
	q := &fakeQueue{pos: queue.Position{Ahead: 2}}
	r := runReq(t, filePlay(q, &fileLoads{}), Request{Files: files})
	want := "Queued **1.mp3**, **2.mp3**, **3.mp3**, **4.mp3** and **5.mp3** (2 ahead of them).\n" +
		"I only take 5 files at a time, so I left out the other 2."
	if r != want || len(q.queued) != 5 {
		t.Errorf("reply = %q, queued %d", r, len(q.queued))
	}
}

func TestPlayFileReplies(t *testing.T) {
	big := mp3("big.mp3")
	big.Size = 1001
	for name, c := range map[string]struct {
		q     Queue
		files []File
		want  string
	}{
		"one, playing next": {&fakeQueue{}, []File{mp3("a.mp3")}, "Getting **a.mp3** ready…"},
		"one, behind":       {&fakeQueue{pos: queue.Position{Ahead: 3}}, []File{mp3("a.mp3")}, "Queued **a.mp3** (3 ahead of it)."},
		"other server":      {&fakeQueue{pos: queue.Position{OtherGuild: true}}, []File{mp3("a.mp3")}, "Queued **a.mp3**. I'm busy playing in other servers"},
		"too big":           {&fakeQueue{}, []File{big}, "Skipped **big.mp3**: it's over the 0 MB limit."},
		"too big and one":   {&fakeQueue{}, []File{big, mp3("a.mp3")}, "Getting **a.mp3** ready…\nSkipped **big.mp3**"},
		"full at once":      {&roomQueue{room: 0}, []File{mp3("a.mp3")}, "The queue is full, so I couldn't add **a.mp3**."},
		"full partway":      {&roomQueue{room: 1}, []File{mp3("a.mp3"), mp3("b.mp3"), mp3("c.mp3")}, "Getting **a.mp3** ready…\nThe queue is full, so I couldn't add the last 2."},
		"markdown in names": {&fakeQueue{}, []File{mp3("*bold*_x_.mp3")}, `Getting **\*bold\*\_x\_.mp3** ready…`},
	} {
		t.Run(name, func(t *testing.T) {
			r := runReq(t, filePlay(c.q, &fileLoads{}), Request{Files: c.files})
			if !strings.Contains(r, c.want) {
				t.Errorf("reply = %q, want containing %q", r, c.want)
			}
		})
	}
}

func TestPlayFilesNeedVoice(t *testing.T) {
	q := &fakeQueue{}
	p := filePlay(q, &fileLoads{})
	p.Voice = fakeLocator{}
	if r := runReq(t, p, Request{Files: []File{mp3("a.mp3")}}); r != "Join a voice channel first, then try again." || len(q.queued) != 0 {
		t.Errorf("reply = %q, queued %d", r, len(q.queued))
	}
}

func TestPlayNonMediaAttachments(t *testing.T) {
	q := &fakeQueue{}
	txt := []File{{URL: "u", Name: "notes.txt", ContentType: "text/plain"}}
	// Alone: say so.
	if r := runReq(t, filePlay(q, &fileLoads{}), Request{Files: txt}); r != "That isn't an audio or video file." {
		t.Errorf("reply = %q", r)
	}
	// A mention with only an image: the same answer.
	if r := runReq(t, filePlay(q, &fileLoads{}), Request{MessageID: 1, Images: []Image{img("i")}}); r != "That isn't an audio or video file." {
		t.Errorf("image: reply = %q", r)
	}
	// A mention with a link: the link plays, the stray attachment is ignored.
	runReq(t, filePlay(q, &fileLoads{}), Request{MessageID: 1, Files: txt, Args: "youtu.be/jNQXAC9IVRw"})
	if len(q.queued) != 1 || q.queued[0].Key != "jNQXAC9IVRw" {
		t.Errorf("queued %+v", q.queued)
	}
	// /play with a query and a chosen file that isn't media: say so, play nothing.
	q = &fakeQueue{}
	if r := runReq(t, filePlay(q, &fileLoads{}), Request{Files: txt, Args: "youtu.be/jNQXAC9IVRw"}); r != "That isn't an audio or video file." || len(q.queued) != 0 {
		t.Errorf("slash: reply = %q, queued %+v", r, q.queued)
	}
}

// "The queue is full" counts only the files that would have been added.
func TestPlayFullQueueCountsOnlyAddable(t *testing.T) {
	big := mp3("big.mp3")
	big.Size = 1001
	r := runReq(t, filePlay(&roomQueue{room: 1}, &fileLoads{}), Request{Files: []File{mp3("a.mp3"), mp3("b.mp3"), big, mp3("c.mp3")}})
	if !strings.Contains(r, "couldn't add the last 2.") {
		t.Errorf("reply = %q", r)
	}
}

// "@Bot play" as a reply plays the replied-to message's files, unless the
// asker attached their own or wrote something to play.
func TestPlayRepliedToFiles(t *testing.T) {
	quote := &Quote{MessageID: 3, Files: []File{mp3("theirs.mp3")}}

	q := &fakeQueue{}
	runReq(t, filePlay(q, &fileLoads{}), Request{Quoted: quote})
	if len(q.queued) != 1 || q.queued[0].Title != "theirs.mp3" {
		t.Errorf("reply to a file: queued %+v", q.queued)
	}

	q = &fakeQueue{}
	runReq(t, filePlay(q, &fileLoads{}), Request{Quoted: quote, Files: []File{mp3("mine.mp3")}})
	if len(q.queued) != 1 || q.queued[0].Title != "mine.mp3" {
		t.Errorf("own file first: queued %+v", q.queued)
	}

	q = &fakeQueue{}
	runReq(t, filePlay(q, &fileLoads{}), Request{Quoted: quote, Args: "youtu.be/jNQXAC9IVRw"})
	if len(q.queued) != 1 || q.queued[0].Key != "jNQXAC9IVRw" {
		t.Errorf("with a link: queued %+v", q.queued)
	}
}

func TestPlayFileLinks(t *testing.T) {
	for link, want := range map[string]string{
		"https://archive.org/download/item/Track%2001.mp3":             "https://archive.org/download/item/Track%2001.mp3",
		"<https://cdn.discordapp.com/attachments/1/2/a.ogg?ex=1&is=2>": "https://cdn.discordapp.com/attachments/1/2/a.ogg?ex=1&is=2",
		"example.com/song.mp3":       "https://example.com/song.mp3",
		"https://vimeo.com/1":        "https://vimeo.com/1", // fetched; a page is refused by its type
		"https://soundcloud.com/x/y": "https://soundcloud.com/x/y",
	} {
		q, loads := &fakeQueue{}, &fileLoads{}
		r := runCmd(t, filePlay(q, loads), link)
		if len(loads.srcs) != 1 || loads.srcs[0].URL != want || len(q.queued) != 1 {
			t.Errorf("%q: loads %+v, reply %q", link, loads.srcs, r.Content)
			continue
		}
		if tr := q.queued[0]; tr.URL != want { // typed links are shown when there's no title
			t.Errorf("%q: track URL %q", link, tr.URL)
		}
	}
}

func TestPlayFileLinkRefusals(t *testing.T) {
	for link, want := range map[string]string{
		"http://localhost:8080/a.mp3":    "private or local address",
		"http://127.0.0.1/a.mp3":         "private or local address",
		"http://[::1]/a.mp3":             "private or local address",
		"http://192.168.1.10/a.mp3":      "private or local address",
		"http://169.254.169.254/latest/": "private or local address",
		"ftp://example.com/a.mp3":        "doesn't look like",
	} {
		q, loads := &fakeQueue{}, &fileLoads{}
		r := runCmd(t, filePlay(q, loads), link)
		if !strings.Contains(r.Content, want) || len(q.queued) != 0 {
			t.Errorf("%q: reply %q, queued %d", link, r.Content, len(q.queued))
		}
	}
}

// YouTube pages that aren't videos, and Spotify links, are never fetched as files.
func TestPlayFileLinksLeaveYouTubeAndSpotify(t *testing.T) {
	for _, link := range []string{"https://www.youtube.com/@channel", "https://youtube.com/feed", "https://open.spotify.com/album/x"} {
		loads := &fileLoads{}
		r := runCmd(t, filePlay(&fakeQueue{}, loads), link)
		if len(loads.srcs) != 0 || !strings.Contains(r.Content, "doesn't look like") {
			t.Errorf("%q: loads %+v, reply %q", link, loads.srcs, r.Content)
		}
	}
}

// Without File set up, files and file links are not played.
func TestPlayWithoutFiles(t *testing.T) {
	p := Play{Voice: fakeLocator{5: 77}, Queue: &fakeQueue{}, YouTube: loaderFor}
	if r := runReq(t, p, Request{Files: []File{mp3("a.mp3")}}); !strings.HasPrefix(r, "Usage: `play <YouTube link or search words>`") {
		t.Errorf("reply = %q", r)
	}
	if r := runCmd(t, p, "https://example.com/a.mp3"); r.Content != "That doesn't look like a YouTube video link." {
		t.Errorf("reply = %q", r.Content)
	}
}

// Discord doesn't pass files in the message box to slash commands, so /play's
// usage points to its file option, and a mention's to attaching.
func TestPlayUsageWithFiles(t *testing.T) {
	p := filePlay(&fakeQueue{}, &fileLoads{})
	if r := runReq(t, p, Request{}); !strings.Contains(r, "`file` option") || !strings.HasPrefix(r, "Usage: `/play ") {
		t.Errorf("slash usage = %q", r)
	}
	if r := runReq(t, p, Request{MessageID: 7}); !strings.Contains(r, "attach audio or video files") || strings.Contains(r, "`file` option") {
		t.Errorf("mention usage = %q", r)
	}
}
