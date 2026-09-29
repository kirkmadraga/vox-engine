package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// PlaybackCommands share play's access (see access.Options.Inherit).
var PlaybackCommands = []string{"test", "queue", "skip", "stop"}

// VoiceLocator finds which voice channel a user is in.
type VoiceLocator interface {
	UserVoiceChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)
}

// Queue is the per-guild playback queue. queue.Manager implements it.
type Queue interface {
	Enqueue(guildID snowflake.ID, t queue.Track) (queue.Position, error)
	Skip(guildID snowflake.ID) (queue.Track, bool)
	Stop(guildID snowflake.ID) bool
	List(guildID snowflake.ID) queue.Listing
}

// Play is "@Bot play <YouTube link>".
type Play struct {
	Voice   VoiceLocator
	Queue   Queue
	YouTube func(videoID string) queue.LoadFunc // loads a video through the cache
}

func (Play) Name() string { return "play" }

func (c Play) Run(ctx context.Context, req Request) error {
	if req.Args == "" {
		return reply(ctx, req, "Usage: `play <YouTube link>`")
	}
	id, err := ytdlp.ParseVideoID(req.Args)
	switch {
	case errors.Is(err, ytdlp.ErrPlaylist):
		return reply(ctx, req, "Playlists aren't supported, only single videos.")
	case err != nil:
		return reply(ctx, req, "That doesn't look like a YouTube video link.")
	}
	return enqueue(ctx, req, c.Voice, c.Queue, queue.Track{
		Key:  id,
		URL:  "https://youtu.be/" + id,
		Load: c.YouTube(id),
	})
}

// Test is "@Bot test": queues the local test tone.
type Test struct {
	Voice VoiceLocator
	Queue Queue
	Tone  queue.LoadFunc
}

func (Test) Name() string { return "test" }

func (c Test) Run(ctx context.Context, req Request) error {
	return enqueue(ctx, req, c.Voice, c.Queue, queue.Track{Title: "Test tone", Load: c.Tone})
}

// enqueue adds t for the requester, who must be in a voice channel.
func enqueue(ctx context.Context, req Request, v VoiceLocator, q Queue, t queue.Track) error {
	channelID, ok := v.UserVoiceChannel(req.GuildID, req.AuthorID)
	if !ok {
		return reply(ctx, req, "Join a voice channel first, then try again.")
	}
	t.RequestedBy, t.VoiceChannel, t.TextChannel = req.AuthorID, channelID, req.ChannelID

	pos, err := q.Enqueue(req.GuildID, t)
	switch {
	case errors.Is(err, queue.ErrFull):
		return reply(ctx, req, "The queue is full. Try again once a few tracks have played.")
	case err != nil:
		return reply(ctx, req, "Couldn't queue that right now. Details are in the bot's log.")
	case pos.Ahead == 0 && pos.OtherGuild:
		return reply(ctx, req, "Queued. I'm playing in another server right now and will start when that finishes.")
	case pos.Ahead == 0:
		return reply(ctx, req, "Getting %s ready…", t.Name())
	}
	return reply(ctx, req, "Queued %s (%d ahead of it).", t.Name(), pos.Ahead)
}

// QueueList is "@Bot queue".
type QueueList struct{ Queue Queue }

func (QueueList) Name() string { return "queue" }

func (c QueueList) Run(ctx context.Context, req Request) error {
	l := c.Queue.List(req.GuildID)
	if l.Current == nil && len(l.Upcoming) == 0 {
		return reply(ctx, req, "The queue is empty.")
	}
	var b strings.Builder
	if l.Current != nil {
		fmt.Fprintf(&b, "**Now playing:** %s\n", describe(*l.Current))
	}
	if len(l.Upcoming) > 0 {
		b.WriteString("**Up next:**\n")
		for i, t := range l.Upcoming {
			fmt.Fprintf(&b, "%d. %s\n", i+1, describe(t))
		}
	}
	return reply(ctx, req, "%s", truncate(strings.TrimRight(b.String(), "\n"), maxMessageLen))
}

func describe(t queue.Track) string {
	s := t.Name()
	if t.Duration > 0 {
		s += " (" + queue.FormatDuration(t.Duration) + ")"
	}
	return s + ", requested by " + Mention(t.RequestedBy)
}

// Skip is "@Bot skip".
type Skip struct{ Queue Queue }

func (Skip) Name() string { return "skip" }

func (c Skip) Run(ctx context.Context, req Request) error {
	t, ok := c.Queue.Skip(req.GuildID)
	if !ok {
		return reply(ctx, req, "Nothing is playing.")
	}
	return reply(ctx, req, "Skipped %s.", t.Name())
}

// Stop is "@Bot stop".
type Stop struct{ Queue Queue }

func (Stop) Name() string { return "stop" }

func (c Stop) Run(ctx context.Context, req Request) error {
	if !c.Queue.Stop(req.GuildID) {
		return reply(ctx, req, "Nothing is playing.")
	}
	return reply(ctx, req, "Stopped and cleared the queue.")
}
