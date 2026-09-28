// Package discordio adapts disgo events and REST calls to the router and commands packages.
package discordio

import (
	"context"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"bot/internal/commands"
	"bot/internal/router"
)

// handleTimeout bounds how long one message may take to process.
const handleTimeout = 30 * time.Second

// MessageSender is the subset of disgo's REST client used to send messages.
type MessageSender interface {
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// GuildMessageHandler returns a disgo listener that feeds guild messages to r.
// base is cancelled on shutdown.
func GuildMessageHandler(base context.Context, r *router.Router, sender MessageSender) func(*events.GuildMessageCreate) {
	return func(e *events.GuildMessageCreate) {
		ctx, cancel := context.WithTimeout(base, handleTimeout)
		defer cancel()
		r.Handle(ctx, router.Message{
			GuildID:   e.GuildID,
			ChannelID: e.ChannelID,
			AuthorID:  e.Message.Author.ID,
			AuthorBot: e.Message.Author.Bot,
			Content:   e.Message.Content,
		}, ChannelReplier{Sender: sender, ChannelID: e.ChannelID})
	}
}

// ChannelReplier sends replies to one channel.
type ChannelReplier struct {
	Sender    MessageSender
	ChannelID snowflake.ID
}

// Reply sends r, pinging only r.Mentions. Explicit empty lists (not nil) tell
// Discord to parse no @everyone/@here/role mentions from the content.
func (c ChannelReplier) Reply(ctx context.Context, r commands.Reply) error {
	users := r.Mentions
	if users == nil {
		users = []snowflake.ID{}
	}
	msg := discord.NewMessageCreate().
		WithContent(r.Content).
		WithAllowedMentions(&discord.AllowedMentions{
			Parse: []discord.AllowedMentionType{},
			Roles: []snowflake.ID{},
			Users: users,
		})
	_, err := c.Sender.CreateMessage(c.ChannelID, msg, rest.WithCtx(ctx))
	return err
}

// notifyTimeout bounds one status message.
const notifyTimeout = 10 * time.Second

// ChannelNotifier posts status messages (e.g. "Now playing") that ping no one.
// It implements queue.Notifier.
type ChannelNotifier struct {
	Sender MessageSender
	Logger *slog.Logger
}

func (n ChannelNotifier) Notify(channelID snowflake.ID, content string) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	if err := (ChannelReplier{Sender: n.Sender, ChannelID: channelID}).Reply(ctx, commands.Reply{Content: content}); err != nil && n.Logger != nil {
		n.Logger.Error("status message failed", "channel", channelID, "err", err)
	}
}

// BotVoiceLeaveHandler returns a disgo listener that calls onLeave when the bot
// itself leaves a voice channel, whether it left on its own or was removed.
// (The queue tells the two apart.)
func BotVoiceLeaveHandler(selfID func() snowflake.ID, onLeave func(guildID snowflake.ID)) func(*events.GuildVoiceLeave) {
	return func(e *events.GuildVoiceLeave) {
		if self := selfID(); self != 0 && e.VoiceState.UserID == self {
			onLeave(e.VoiceState.GuildID)
		}
	}
}
