package router

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/commands"
)

// Message is the part of an incoming Discord message the router needs.
type Message struct {
	GuildID    snowflake.ID
	ChannelID  snowflake.ID
	AuthorID   snowflake.ID
	AuthorBot  bool
	AuthorName string // display name (server nickname if set)
	MessageID  snowflake.ID
	ReplyToID  snowflake.ID // the message this one replies to (Discord reply), or 0
	Content    string
	Mentions   []snowflake.ID   // users the message pings (a reply's @ ping included)
	Images     []commands.Image // attached images, in order
	Quoted     *commands.Quote  // the replied-to message, when Discord included it
}

// Router turns "@Bot <command> <args>" messages into command calls.
type Router struct {
	selfID   func() snowflake.ID // the bot's user ID; 0 until the gateway is ready
	registry *commands.Registry
	access   access.Checker
	logger   *slog.Logger

	// ThreadParent, if set, returns a thread's parent channel (0 if channelID
	// isn't a known thread), so a thread counts as its parent for channel limits.
	ThreadParent func(channelID snowflake.ID) snowflake.ID

	// IsAnswer, if set, reports whether a message is one of the fallback
	// command's answers: a Discord reply to one that pings the bot (the
	// reply's @ ping on) runs the fallback with the whole reply as its args,
	// as if the bot had been mentioned. Replies with the ping off are ignored.
	IsAnswer func(channelID, messageID snowflake.ID) bool

	// Fallback, if set and registered, receives mentions whose first word isn't
	// a command, with the whole text as its args (and is access-checked as
	// itself). Unset or unregistered: such mentions get "unknown command".
	Fallback string
}

// New builds a Router.
func New(selfID func() snowflake.ID, registry *commands.Registry, checker access.Checker, logger *slog.Logger) *Router {
	return &Router{selfID: selfID, registry: registry, access: checker, logger: logger}
}

// Handle processes one message. Messages that are not for the bot, or that the
// author may not run, are ignored without any reply. Ignore reasons are logged at
// debug level; message content is never logged.
func (r *Router) Handle(ctx context.Context, msg Message, reply commands.Replier) {
	log := r.logger.With("user", msg.AuthorID, "guild", msg.GuildID, "channel", msg.ChannelID)
	if msg.AuthorBot {
		return // not logged: bots (including this one) would flood the debug log
	}
	self := r.selfID()
	if self == 0 {
		log.Debug("router: ignored", "reason", "not ready")
		return
	}
	if q := msg.Quoted; q != nil {
		cp := *q
		cp.Self = q.AuthorID == self
		msg.Quoted = &cp
	}
	text, reason := afterMention(msg.Content, self)
	pinged := slices.Contains(msg.Mentions, self)
	if reason == reasonNoMention && pinged && r.replyToAnswer(msg) {
		// Continuing a conversation: the whole reply is the question.
		r.dispatch(ctx, log, msg, r.Fallback, strings.TrimSpace(msg.Content), true, reply)
		return
	}
	if reason != "" {
		// With the Message Content intent every chat message arrives here;
		// only log those that were at least meant for the bot.
		if pinged || (reason != reasonNoMention && reason != reasonEmpty) {
			log.Debug("router: ignored", "reason", reason, "content_len", len(msg.Content))
		}
		return
	}
	name, args := splitCommand(text)
	fallback := false
	if _, known := r.registry.Lookup(name); !known && r.Fallback != "" {
		if _, ok := r.registry.Lookup(r.Fallback); ok {
			name, args = r.Fallback, text // "@Bot What is jazz?": the whole text, as typed
			fallback = true
		}
	}
	r.dispatch(ctx, log, msg, name, args, fallback, reply)
}

// replyToAnswer reports whether msg is a Discord reply to one of the
// fallback command's answers, and the fallback is available. An answer is
// one IsAnswer knows (recent, since the last start), or any message of this
// bot's that is itself a reply: only ask answers are sent as replies (to
// their question), so this also covers answers from before a restart.
func (r *Router) replyToAnswer(msg Message) bool {
	if msg.ReplyToID == 0 || r.Fallback == "" {
		return false
	}
	known := r.IsAnswer != nil && r.IsAnswer(msg.ChannelID, msg.ReplyToID)
	shaped := msg.Quoted != nil && msg.Quoted.Self && msg.Quoted.IsReply && msg.Quoted.MessageID == msg.ReplyToID
	if !known && !shaped {
		return false
	}
	_, ok := r.registry.Lookup(r.Fallback)
	return ok
}

// dispatch checks access for command name and runs it. fallback marks an
// invocation the user didn't name explicitly: refused in the wrong channel,
// it stays silent instead of hinting.
func (r *Router) dispatch(ctx context.Context, log *slog.Logger, msg Message, name, args string, fallback bool, reply commands.Replier) {
	inv := Invocation{GuildID: msg.GuildID, ChannelID: msg.ChannelID, AuthorID: msg.AuthorID, AuthorName: msg.AuthorName,
		MessageID: msg.MessageID, Name: name, Args: args, Images: msg.Images, Quoted: msg.Quoted}
	switch r.Check(ctx, inv) {
	case Denied:
		return // silent: don't advertise the bot to unauthorized users
	case WrongChannel:
		// Only an explicit "ask" gets the hint: ordinary chat in other
		// channels ("@Bot hi") must not draw a reply.
		if !fallback {
			if err := reply.Reply(ctx, commands.Reply{Content: WrongChannelMessage(inv.Name)}); err != nil {
				log.Error("router: reply failed", "err", err)
			}
		}
		return
	}
	r.Run(ctx, inv, reply)
}

// Verdict is the access policy's answer for one invocation.
type Verdict int

const (
	Denied       Verdict = iota // not allowed at all: stay silent
	Allowed                     // run it
	WrongChannel                // allowed, but not in this channel
)

// WrongChannelMessage tells a user who may run command that it's not enabled
// where they tried it. It doesn't list the allowed channels: the user may not
// be able to see them.
func WrongChannelMessage(command string) string {
	return "`" + command + "` isn't enabled in this channel."
}

// Invocation is one command call, from a mention or a slash command.
type Invocation struct {
	GuildID    snowflake.ID
	ChannelID  snowflake.ID
	AuthorID   snowflake.ID
	AuthorName string           // display name (server nickname if set)
	MessageID  snowflake.ID     // the asking message; 0 for slash commands
	Name       string           // lowercased command name
	Args       string           // the command's argument text, as typed after the name
	Lucky      bool             // /play's lucky option (slash only; see commands.Request)
	Search     bool             // /ask's search option (slash only)
	Images     []commands.Image // the asker's attached images (mentions only)
	Quoted     *commands.Quote  // the message replied to, if Discord included it
}

// Check applies the access policy to inv (the same for every way in): first
// the guild and grant rules, then channel limits. A refusal is logged at debug
// level.
func (r *Router) Check(ctx context.Context, inv Invocation) Verdict {
	log := func(reason string) {
		r.logger.Debug("router: ignored", "reason", reason, "user", inv.AuthorID, "guild", inv.GuildID, "channel", inv.ChannelID, "command", inv.Name)
	}
	if !r.access.Allowed(ctx, inv.AuthorID, inv.GuildID, inv.Name) {
		log("not allowed")
		return Denied
	}
	var parent snowflake.ID
	if r.ThreadParent != nil {
		parent = r.ThreadParent(inv.ChannelID)
	}
	if !r.access.ChannelAllowed(ctx, inv.AuthorID, inv.Name, inv.ChannelID, parent) {
		log("not enabled in this channel")
		return WrongChannel
	}
	return Allowed
}

// Run runs an allowed invocation's command, replying through reply.
func (r *Router) Run(ctx context.Context, inv Invocation, reply commands.Replier) {
	log := r.logger.With("user", inv.AuthorID, "guild", inv.GuildID, "channel", inv.ChannelID, "command", inv.Name)
	cmd, found := r.registry.Lookup(inv.Name)
	if !found {
		log.Debug("router: unknown command")
		if err := reply.Reply(ctx, commands.Reply{Content: "unknown command"}); err != nil {
			log.Error("router: reply failed", "err", err)
		}
		return
	}

	log.Info("router: dispatch")
	err := cmd.Run(ctx, commands.Request{
		GuildID:    inv.GuildID,
		ChannelID:  inv.ChannelID,
		AuthorID:   inv.AuthorID,
		AuthorName: inv.AuthorName,
		MessageID:  inv.MessageID,
		Args:       inv.Args,
		Reply:      reply,
		Lucky:      inv.Lucky,
		Search:     inv.Search,
		Images:     inv.Images,
		Quoted:     inv.Quoted,
	})
	if err != nil {
		log.Error("router: command failed", "err", err)
	}
}

// Reasons parse gives for ignoring a message.
const (
	reasonEmpty     = "empty content (no Message Content intent and the bot user was not mentioned?)"
	reasonNoMention = "no leading mention"
	reasonOtherUser = "leading mention is another user"
	reasonRole      = "leading mention is a role (only direct mentions of the bot user count)"
	reasonNoCommand = "no command word after mention"
)

// parse extracts the command name (lowercased) and args from content that starts
// with a direct mention of the bot user (<@id> or <@!id>). Role mentions, including
// the bot's own managed role, are ignored. A non-empty reason means "ignore".
func parse(content string, selfID snowflake.ID) (name, args, reason string) {
	text, reason := afterMention(content, selfID)
	if reason != "" {
		return "", "", reason
	}
	name, args = splitCommand(text)
	return name, args, ""
}

// afterMention returns the trimmed text after a leading direct mention of the
// bot user. A non-empty reason means "ignore".
func afterMention(content string, selfID snowflake.ID) (text, reason string) {
	s := strings.TrimLeftFunc(content, unicode.IsSpace)
	if s == "" {
		return "", reasonEmpty
	}
	role, id, rest, ok := leadingMention(s)
	switch {
	case !ok:
		return "", reasonNoMention
	case role:
		return "", reasonRole
	case id != selfID:
		return "", reasonOtherUser
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", reasonNoCommand
	}
	return rest, ""
}

// splitCommand splits non-empty text into its first word (lowercased) and the rest.
func splitCommand(text string) (name, args string) {
	word, remainder := text, ""
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		word, remainder = text[:i], text[i:]
	}
	return strings.ToLower(word), strings.TrimSpace(remainder)
}

// leadingMention parses a <@id>, <@!id> or <@&id> mention at the start of s.
// Role mentions are recognised only so the debug log can say why they were ignored.
func leadingMention(s string) (role bool, id snowflake.ID, rest string, ok bool) {
	if !strings.HasPrefix(s, "<@") {
		return false, 0, "", false
	}
	end := strings.IndexByte(s, '>')
	if end < 0 {
		return false, 0, "", false
	}
	inner := s[2:end]
	switch {
	case strings.HasPrefix(inner, "&"):
		role, inner = true, inner[1:]
	case strings.HasPrefix(inner, "!"):
		inner = inner[1:]
	}
	parsed, err := snowflake.Parse(inner)
	if err != nil {
		return false, 0, "", false
	}
	return role, parsed, s[end+1:], true
}
