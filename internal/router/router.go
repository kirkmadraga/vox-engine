package router

import (
	"context"
	"log/slog"
	"strings"
	"unicode"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/commands"
)

// Message is the part of an incoming Discord message the router needs.
type Message struct {
	GuildID   snowflake.ID
	ChannelID snowflake.ID
	AuthorID  snowflake.ID
	AuthorBot bool
	Content   string
}

// Router turns "@Bot <command> <args>" messages into command calls.
type Router struct {
	selfID   func() snowflake.ID // the bot's user ID; 0 until the gateway is ready
	registry *commands.Registry
	access   access.Checker
	logger   *slog.Logger
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
		log.Debug("ignored", "reason", "not ready")
		return
	}
	name, args, reason := parse(msg.Content, self)
	if reason != "" {
		log.Debug("ignored", "reason", reason, "content_len", len(msg.Content))
		return
	}
	inv := Invocation{GuildID: msg.GuildID, ChannelID: msg.ChannelID, AuthorID: msg.AuthorID, Name: name, Args: args}
	if !r.Allowed(ctx, inv) {
		return // silent: don't advertise the bot to unauthorized users
	}
	r.Run(ctx, inv, reply)
}

// Invocation is one command call, from a mention or a slash command.
type Invocation struct {
	GuildID   snowflake.ID
	ChannelID snowflake.ID
	AuthorID  snowflake.ID
	Name      string // lowercased command name
	Args      string // the command's argument text, as typed after the name
}

// Allowed applies the access policy to inv (the same for every way in). A
// refusal is logged at debug level.
func (r *Router) Allowed(ctx context.Context, inv Invocation) bool {
	if r.access.Allowed(ctx, inv.AuthorID, inv.GuildID, inv.Name) {
		return true
	}
	r.logger.Debug("ignored", "reason", "not allowed", "user", inv.AuthorID, "guild", inv.GuildID, "channel", inv.ChannelID, "command", inv.Name)
	return false
}

// Run runs an allowed invocation's command, replying through reply.
func (r *Router) Run(ctx context.Context, inv Invocation, reply commands.Replier) {
	log := r.logger.With("user", inv.AuthorID, "guild", inv.GuildID, "channel", inv.ChannelID, "command", inv.Name)
	cmd, found := r.registry.Lookup(inv.Name)
	if !found {
		log.Debug("unknown command")
		if err := reply.Reply(ctx, commands.Reply{Content: "unknown command"}); err != nil {
			log.Error("reply failed", "err", err)
		}
		return
	}

	log.Info("dispatch")
	err := cmd.Run(ctx, commands.Request{
		GuildID:   inv.GuildID,
		ChannelID: inv.ChannelID,
		AuthorID:  inv.AuthorID,
		Args:      inv.Args,
		Reply:     reply,
	})
	if err != nil {
		log.Error("command failed", "err", err)
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
	s := strings.TrimLeftFunc(content, unicode.IsSpace)
	if s == "" {
		return "", "", reasonEmpty
	}
	role, id, rest, ok := leadingMention(s)
	switch {
	case !ok:
		return "", "", reasonNoMention
	case role:
		return "", "", reasonRole
	case id != selfID:
		return "", "", reasonOtherUser
	}

	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", reasonNoCommand
	}
	word, remainder := rest, ""
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		word, remainder = rest[:i], rest[i:]
	}
	return strings.ToLower(word), strings.TrimSpace(remainder), ""
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
