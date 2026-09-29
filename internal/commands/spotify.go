package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/spotify"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// spotifyCandidates is how many regular YouTube results are scored (and
// listed, if none is a confident match).
const spotifyCandidates = 5

// playSpotify plays a Spotify track link: it reads the track's title, artists
// and length from its public page, then finds the same recording on YouTube:
//
//  1. YouTube Music song search: an exact title near the top is the official
//     recording.
//  2. Otherwise, regular YouTube search, scored on length, channel and title
//     (spotify.Best).
//  3. Otherwise, the regular results are listed for "play <number>".
//
// Only metadata comes from Spotify; audio is always YouTube's.
func (c Play) playSpotify(ctx context.Context, req Request) error {
	id, err := spotify.ParseTrackID(req.Args)
	switch {
	case errors.Is(err, spotify.ErrNotTrack):
		return reply(ctx, req, "Only single Spotify tracks are supported for now, not albums, playlists or artists.")
	case errors.Is(err, spotify.ErrShortLink):
		return reply(ctx, req, "Short spotify.link links aren't supported yet. Open it and use the full open.spotify.com link.")
	case err != nil:
		return reply(ctx, req, "That doesn't look like a Spotify track link.")
	}
	// Check voice first so no lookup is wasted.
	if _, ok := c.Voice.UserVoiceChannel(req.GuildID, req.AuthorID); !ok {
		return reply(ctx, req, "Join a voice channel first, then try again.")
	}

	track, err := c.Spotify(ctx, id)
	switch {
	case errors.Is(err, spotify.ErrNotFound):
		return reply(ctx, req, "Couldn't find that Spotify track.")
	case err != nil:
		return errors.Join(reply(ctx, req, "Couldn't read that Spotify link. Details are in the bot's log."), fmt.Errorf("spotify %s: %w", id, err))
	}
	query := track.Artists + " " + track.Title

	log := c.logger().With("spotify", id, "track", track.Name())

	// 1. YouTube Music. A failure here isn't fatal: step 2 still runs.
	if c.SearchMusic != nil {
		hits, err := c.SearchMusic(ctx, query, spotify.ExactTitleTop)
		if err != nil {
			log.Warn("spotify: YouTube Music search failed; trying regular search", "err", err)
		} else if h, ok := spotify.ExactTitle(track, hits); ok {
			log.Info("spotify: matched", "step", "youtube-music", "video", h.ID, "title", h.Title)
			return c.enqueueSpotify(ctx, req, track, h)
		}
	}

	// 2. Regular YouTube, scored.
	hits, err := c.Search(ctx, query, spotifyCandidates)
	if err == nil && len(hits) == 0 {
		err = ytdlp.ErrNoResults
	}
	if err != nil {
		return searchFailed(ctx, req, err)
	}
	if h, ok := spotify.Best(track, hits); ok {
		score, _ := spotify.Score(track, h)
		log.Info("spotify: matched", "step", "scored", "video", h.ID, "title", h.Title, "channel", h.Channel, "score", score)
		return c.enqueueSpotify(ctx, req, track, h)
	}

	// 3. No confident match: let the caller pick.
	log.Info("spotify: no sure match; listed results", "results", len(hits))
	c.Recent.Put(req.GuildID, req.AuthorID, hits)
	msg := fmt.Sprintf("Couldn't find a sure match for **%s** on YouTube.\n%s", queue.EscapeMarkdown(track.Name()), formatResults(hits))
	return reply(ctx, req, "%s", truncate(msg, maxMessageLen))
}

func (c Play) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// enqueueSpotify queues YouTube video h under the Spotify track's name.
func (c Play) enqueueSpotify(ctx context.Context, req Request, t spotify.Track, h ytdlp.Hit) error {
	return c.enqueueVideo(ctx, req, ytdlp.Hit{ID: h.ID, Title: t.Name(), Duration: t.Duration})
}
