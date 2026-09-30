// Package discordio adapts disgo events and REST calls to the router and commands packages.
package discordio

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/router"
)

// HandleTimeout bounds how long one message or slash command may take to
// process. main raises it when ask is on, to cover ask's own time limit plus
// time to send the reply.
var HandleTimeout = 30 * time.Second

// MessageSender is the subset of disgo's REST client used to send messages.
type MessageSender interface {
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// GuildMessageHandler returns a disgo listener that feeds guild messages to r.
// base is cancelled on shutdown. typing (optional) shows "typing…" for
// commands that ask for it.
func GuildMessageHandler(base context.Context, r *router.Router, sender MessageSender, typing *ChannelTyping) func(*events.GuildMessageCreate) {
	return func(e *events.GuildMessageCreate) {
		ctx, cancel := context.WithTimeout(base, HandleTimeout)
		defer cancel()
		r.Handle(ctx, router.Message{
			GuildID:    e.GuildID,
			ChannelID:  e.ChannelID,
			AuthorID:   e.Message.Author.ID,
			AuthorBot:  e.Message.Author.Bot,
			AuthorName: displayName(e.Message),
			ReplyToID:  repliedTo(e.Message),
			Mentions:   mentionIDs(e.Message),
			Images:     images(e.Message.Attachments),
			Quoted:     quoted(e.Message),
			MessageID:  e.MessageID,
			Content:    e.Message.Content,
		}, ChannelReplier{Sender: sender, ChannelID: e.ChannelID, Typing: typing})
	}
}

// mentionIDs are the users m pings (a reply's @ ping included).
func mentionIDs(m discord.Message) []snowflake.ID {
	ids := make([]snowflake.ID, 0, len(m.Mentions))
	for _, u := range m.Mentions {
		ids = append(ids, u.ID)
	}
	return ids
}

// images keeps the attachments Discord marks as images, in order.
func images(atts []discord.Attachment) []commands.Image {
	var out []commands.Image
	for _, a := range atts {
		if a.ContentType == nil || !strings.HasPrefix(*a.ContentType, "image/") {
			continue
		}
		out = append(out, commands.Image{URL: a.URL, ContentType: *a.ContentType, Size: a.Size})
	}
	return out
}

// quoted is the message m replies to, if Discord included it. Without the
// Message Content intent, other people's messages arrive blank, so nil then.
func quoted(m discord.Message) *commands.Quote {
	ref := m.ReferencedMessage
	if repliedTo(m) == 0 || ref == nil {
		return nil
	}
	q := &commands.Quote{
		MessageID: ref.ID, AuthorID: ref.Author.ID, AuthorName: displayName(*ref), AuthorBot: ref.Author.Bot,
		IsReply: ref.Type == discord.MessageTypeReply, Content: ref.Content, Images: images(ref.Attachments),
	}
	if q.Content == "" && len(q.Images) == 0 {
		return nil
	}
	return q
}

// repliedTo is the message m replies to, or 0.
func repliedTo(m discord.Message) snowflake.ID {
	if m.Type == discord.MessageTypeReply && m.MessageReference != nil && m.MessageReference.MessageID != nil {
		return *m.MessageReference.MessageID
	}
	return 0
}

// displayName is the author's server nickname, else their Discord display name.
// (A message's Member has no User, so Member.EffectiveName can't fall back.)
func displayName(m discord.Message) string {
	if m.Member != nil && m.Member.Nick != nil {
		return *m.Member.Nick
	}
	return m.Author.EffectiveName()
}

// MessageDeleteHandler returns a disgo listener that reports deleted guild
// messages as forget(channelID, messageID).
func MessageDeleteHandler(forget func(channelID, messageID snowflake.ID)) func(*events.GuildMessageDelete) {
	return func(e *events.GuildMessageDelete) { forget(e.ChannelID, e.MessageID) }
}

// ChannelReplier sends replies to one channel. Replies can't be private there,
// so Reply.Private is ignored.
type ChannelReplier struct {
	Sender    MessageSender
	ChannelID snowflake.ID
	Typing    *ChannelTyping // nil: no typing indicator
}

// StartTyping implements commands.TypingIndicator.
func (c ChannelReplier) StartTyping() (stop func()) {
	if c.Typing == nil {
		return func() {}
	}
	return c.Typing.Start(c.ChannelID)
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
	if r.ReplyTo != 0 {
		msg.AllowedMentions.RepliedUser = r.PingReplied
		ref := r.ReplyTo
		msg.MessageReference = &discord.MessageReference{MessageID: &ref}
		if sent, err := c.Sender.CreateMessage(c.ChannelID, msg, rest.WithCtx(ctx)); err == nil {
			reportSent(r, sent)
			return nil
		}
		// Most likely the question was deleted meanwhile (Discord refuses
		// replies to missing messages): send it as a plain message instead.
		msg.MessageReference = nil
		msg.AllowedMentions.RepliedUser = false
	}
	sent, err := c.Sender.CreateMessage(c.ChannelID, msg, rest.WithCtx(ctx))
	if err == nil {
		reportSent(r, sent)
	}
	return err
}

// reportSent tells r.Sent (if any) which message was sent.
func reportSent(r commands.Reply, sent *discord.Message) {
	if r.Sent != nil && sent != nil {
		r.Sent(sent.ID)
	}
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
