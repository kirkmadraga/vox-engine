package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kirkmadraga/vox-engine/internal/mediafile"
	"github.com/kirkmadraga/vox-engine/internal/queue"
)

// MaxFilesPerPlay is how many attached files one play queues.
const MaxFilesPerPlay = 5

// mediaFiles are the files play would queue for req: the asker's audio and
// video attachments, else, when play has no text, those of the message it
// replies to. attached reports whether there were attachments at all,
// images included.
func mediaFiles(req Request) (files []File, attached bool) {
	pick := func(all []File) []File {
		var out []File
		for _, f := range all {
			if mediafile.IsMedia(f.Name, f.ContentType) {
				out = append(out, f)
			}
		}
		return out
	}
	if len(req.Files) > 0 || len(req.Images) > 0 {
		return pick(req.Files), true
	}
	if req.Args == "" && req.Quoted != nil && len(req.Quoted.Files) > 0 {
		return pick(req.Quoted.Files), true
	}
	return nil, false
}

// fileTrack is one file to queue.
type fileTrack struct {
	src  mediafile.Source
	link bool // a link the asker typed (shown and logged); attachments' links never are
}

// playAttachments queues up to MaxFilesPerPlay attached files.
func (c Play) playAttachments(ctx context.Context, req Request, files []File) error {
	var tracks []fileTrack
	for _, f := range files {
		tracks = append(tracks, fileTrack{src: mediafile.Source{URL: f.URL, Name: f.Name, Size: f.Size}})
	}
	return c.playFiles(ctx, req, tracks)
}

// playLink queues a direct link to a file.
func (c Play) playLink(ctx context.Context, req Request) error {
	link, err := mediafile.CheckLink(req.Args)
	if err != nil {
		return reply(ctx, req, "%s", queue.UserMessage(err))
	}
	return c.playFiles(ctx, req, []fileTrack{{src: mediafile.Source{URL: link}, link: true}})
}

// playFiles queues tracks for the asker, answering once for all of them.
func (c Play) playFiles(ctx context.Context, req Request, tracks []fileTrack) error {
	channelID, ok := c.Voice.UserVoiceChannel(req.GuildID, req.AuthorID)
	if !ok {
		return reply(ctx, req, "Join a voice channel first, then try again.")
	}
	var notes []string
	if len(tracks) > MaxFilesPerPlay {
		notes = append(notes, fmt.Sprintf("I only take %d files at a time, so I left out the other %d.", MaxFilesPerPlay, len(tracks)-MaxFilesPerPlay))
		tracks = tracks[:MaxFilesPerPlay]
	}

	var queued []string
	first := queue.Position{}
	for i, ft := range tracks {
		t := queue.Track{Title: mediafile.Title(ft.src), Key: mediafile.NewKey(),
			RequestedBy: req.AuthorID, VoiceChannel: channelID, TextChannel: req.ChannelID}
		if ft.link {
			t.URL = ft.src.URL
		}
		if c.MaxFileBytes > 0 && ft.src.Size > c.MaxFileBytes {
			notes = append(notes, fmt.Sprintf("Skipped %s: it's over the %s limit.", t.Name(), mediafile.FormatBytes(c.MaxFileBytes)))
			continue
		}
		t.Load = c.File(t.Key, ft.src)
		pos, err := c.Queue.Enqueue(req.GuildID, t)
		if errors.Is(err, queue.ErrFull) {
			left := 0 // this one and the rest, except those too big anyway
			for _, rest := range tracks[i:] {
				if c.MaxFileBytes <= 0 || rest.src.Size <= c.MaxFileBytes {
					left++
				}
			}
			if left > 1 {
				notes = append(notes, fmt.Sprintf("The queue is full, so I couldn't add the last %d.", left))
			} else {
				notes = append(notes, "The queue is full, so I couldn't add "+t.Name()+".")
			}
			break
		}
		if err != nil {
			notes = append(notes, "Couldn't queue "+t.Name()+" right now. Details are in the bot's log.")
			break
		}
		if len(queued) == 0 {
			first = pos
		}
		queued = append(queued, t.Name())
	}

	var b strings.Builder
	switch {
	case len(queued) == 0:
	case first.Ahead == 0 && first.OtherGuild:
		fmt.Fprintf(&b, "Queued %s. I'm busy playing in other servers right now and will start as soon as I'm free.", joinNames(queued))
	case first.Ahead == 0:
		fmt.Fprintf(&b, "Getting %s ready…", joinNames(queued))
	case len(queued) == 1:
		fmt.Fprintf(&b, "Queued %s (%d ahead of it).", queued[0], first.Ahead)
	default:
		fmt.Fprintf(&b, "Queued %s (%d ahead of them).", joinNames(queued), first.Ahead)
	}
	for _, n := range notes {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(n)
	}
	return reply(ctx, req, "%s", truncate(b.String(), maxMessageLen))
}

// joinNames lists names as "a", "a and b", or "a, b and c".
func joinNames(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
