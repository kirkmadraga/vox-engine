package commands

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"bot/internal/voice"
)

// VoiceLocator finds which voice channel a user is in.
type VoiceLocator interface {
	UserVoiceChannel(guildID, userID snowflake.ID) (snowflake.ID, bool)
}

// AudioPlayer starts background playback of a local Ogg Opus file.
// voice.Player implements it.
type AudioPlayer interface {
	Start(guildID, channelID snowflake.ID, path string, done func(error)) error
}

// statusTimeout bounds the reply sent after background playback ends.
const statusTimeout = 10 * time.Second

// Play is "@Bot play". For now it plays a local test tone (V3 of the voice
// plan); YouTube links come later.
type Play struct {
	Voice    VoiceLocator
	Player   AudioPlayer
	TonePath string
	Logger   *slog.Logger
}

func (Play) Name() string { return "play" }

func (c Play) Run(ctx context.Context, req Request) error {
	if req.Args != "" {
		return reply(ctx, req, "Only the test tone works for now: send `play` with nothing after it. YouTube links are coming next.")
	}
	channelID, ok := c.Voice.UserVoiceChannel(req.GuildID, req.AuthorID)
	if !ok {
		return reply(ctx, req, "Join a voice channel first, then try again.")
	}

	err := c.Player.Start(req.GuildID, channelID, c.TonePath, func(err error) {
		// Playback ended long after this command returned, so use a fresh context.
		if err == nil || errors.Is(err, context.Canceled) {
			return // finished, or the bot is shutting down
		}
		c.logger().Error("playback failed", "guild", req.GuildID, "channel", channelID, "err", err)
		msg := "Playback failed, so I left the channel. Details are in the bot's log."
		if errors.Is(err, voice.ErrNotReady) {
			msg = "Voice encryption (DAVE) didn't finish setting up, so I left the channel. Try again in a moment."
		}
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		defer cancel()
		_ = reply(ctx, req, "%s", msg)
	})
	switch {
	case errors.Is(err, voice.ErrBusy):
		return reply(ctx, req, "I'm already playing something. Try again when it finishes.")
	case errors.Is(err, fs.ErrNotExist):
		c.logger().Error("test tone missing", "path", c.TonePath)
		return reply(ctx, req, "The test tone file is missing on the bot's machine (see the log for the path).")
	case err != nil:
		c.logger().Error("playback could not start", "path", c.TonePath, "err", err)
		return reply(ctx, req, "Couldn't start playback. Details are in the bot's log.")
	}
	return reply(ctx, req, "Playing the test tone in <#%s>.", channelID)
}

func (c Play) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}
