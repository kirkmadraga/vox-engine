package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/spotify"
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

// Play is "@Bot play <YouTube link | Spotify track link | search words | number>".
// YouTube links are played directly; Spotify tracks are matched to a YouTube
// video (see playSpotify). Search words list the top YouTube results,
// remembered for the caller; "play <number>" then queues one of them.
type Play struct {
	Voice       VoiceLocator
	Queue       Queue
	YouTube     func(videoID string) queue.LoadFunc                                 // loads a video through the cache
	Search      func(ctx context.Context, query string, n int) ([]ytdlp.Hit, error) // top YouTube results; nil = links only
	Results     int                                                                 // how many results search words list
	Recent      *RecentSearches                                                     // each caller's last results, for "play <number>"
	Spotify     func(ctx context.Context, trackID string) (spotify.Track, error)    // Spotify track details; nil = no Spotify links
	SearchMusic func(ctx context.Context, query string, n int) ([]ytdlp.Hit, error) // YouTube Music songs; nil = skip that step
	Logger      *slog.Logger                                                        // optional: how Spotify tracks were matched
}

func (Play) Name() string { return "play" }

func (c Play) Run(ctx context.Context, req Request) error {
	if req.Args == "" {
		return reply(ctx, req, "Usage: `play <YouTube link or search words>`, then `play <number>` to pick a result")
	}
	if n, ok := parsePick(req.Args); ok {
		return c.pick(ctx, req, n)
	}
	id, err := ytdlp.ParseVideoID(req.Args)
	switch {
	case err == nil:
		return c.enqueueVideo(ctx, req, ytdlp.Hit{ID: id})
	case errors.Is(err, ytdlp.ErrPlaylist):
		return reply(ctx, req, "Playlists aren't supported, only single videos.")
	case spotify.IsLink(req.Args) && c.Spotify != nil && c.Search != nil && c.Recent != nil:
		return c.playSpotify(ctx, req)
	case ytdlp.LooksLikeLink(req.Args), spotify.IsLink(req.Args), c.Search == nil, c.Recent == nil:
		return reply(ctx, req, "That doesn't look like a YouTube video link.")
	}

	// Search words: list the results; nothing is queued until "play <number>".
	hits, err := c.Search(ctx, req.Args, c.Results)
	if err == nil && len(hits) == 0 {
		err = ytdlp.ErrNoResults
	}
	if err != nil {
		return searchFailed(ctx, req, err)
	}
	c.Recent.Put(req.GuildID, req.AuthorID, hits)
	return reply(ctx, req, "%s", formatResults(hits))
}

// pick queues result n from the caller's last search.
func (c Play) pick(ctx context.Context, req Request, n int) error {
	var hits []ytdlp.Hit
	ok := false
	if c.Recent != nil {
		hits, ok = c.Recent.Get(req.GuildID, req.AuthorID)
	}
	switch {
	case !ok:
		return reply(ctx, req, "No recent search to pick from. Use `play <search words>` first, then `play <number>`.")
	case n < 1 || n > len(hits):
		return reply(ctx, req, "Pick a number from 1 to %d.", len(hits))
	}
	return c.enqueueVideo(ctx, req, hits[n-1])
}

// parsePick reads "play <number>": one to three ASCII digits, nothing else.
func parsePick(s string) (int, bool) {
	if len(s) == 0 || len(s) > 3 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func (c Play) enqueueVideo(ctx context.Context, req Request, v ytdlp.Hit) error {
	return enqueue(ctx, req, c.Voice, c.Queue, queue.Track{
		Key:      v.ID,
		URL:      "https://youtu.be/" + v.ID,
		Title:    v.Title,
		Duration: v.Duration,
		Load:     c.YouTube(v.ID),
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
