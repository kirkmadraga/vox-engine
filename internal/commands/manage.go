package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
)

// ManagementCommands are owner-only and can never be granted.
var ManagementCommands = []string{"allow", "deny", DebugCommand}

// DefaultGrantCommand is granted or revoked when "allow/deny @user" names no command.
const DefaultGrantCommand = "play"

// maxMessageLen is Discord's message length limit, in characters.
const maxMessageLen = 2000

// AccessManager is what the management commands need. access.Policy implements it.
type AccessManager interface {
	IsOwner(userID snowflake.ID) bool
	InheritsFrom(command string) (parent string, ok bool)
	GuildAllowed(ctx context.Context, guildID snowflake.ID) (bool, error)
	AllowGuild(ctx context.Context, guildID, by snowflake.ID) (bool, error)
	DenyGuild(ctx context.Context, guildID, by snowflake.ID) (bool, error)
	Grant(ctx context.Context, userID snowflake.ID, command string, by snowflake.ID) (bool, error)
	Revoke(ctx context.Context, userID snowflake.ID, command string, by snowflake.ID) (bool, error)
	AllowChannel(ctx context.Context, channelID snowflake.ID, command string, by snowflake.ID) (bool, error)
	DenyChannel(ctx context.Context, channelID snowflake.ID, command string, by snowflake.ID) (bool, error)
	Snapshot(ctx context.Context) (access.Snapshot, error)
}

const saveFailed = "Couldn't save that change, so nothing changed. Details are in the bot's log."

// target is what "allow"/"deny" act on: a guild (guildID 0 means the current
// one), a channel for ask (channelID 0 means the current one), or a user and
// a command.
type target struct {
	guild     bool
	guildID   snowflake.ID
	channel   bool
	channelID snowflake.ID
	user      snowflake.ID
	command   string
}

// parseTarget parses "guild [guildID]", "ask [#channel | channelID]" or
// "<@id> [command]".
func parseTarget(args string) (target, bool) {
	fields := strings.Fields(args)
	if len(fields) == 0 || len(fields) > 2 {
		return target{}, false
	}
	if strings.EqualFold(fields[0], AskCommand) {
		t := target{channel: true, command: AskCommand}
		if len(fields) == 2 {
			id, ok := parseChannel(fields[1])
			if !ok {
				return target{}, false
			}
			t.channelID = id
		}
		return t, true
	}
	if strings.EqualFold(fields[0], "guild") {
		if len(fields) == 1 {
			return target{guild: true}, true
		}
		id, err := snowflake.Parse(fields[1])
		if err != nil || id == 0 {
			return target{}, false
		}
		return target{guild: true, guildID: id}, true
	}
	user, ok := parseUserMention(fields[0])
	if !ok {
		return target{}, false
	}
	t := target{user: user, command: DefaultGrantCommand}
	if len(fields) == 2 {
		t.command = strings.ToLower(fields[1])
	}
	return t, true
}

// parseUserMention parses exactly "<@id>" or "<@!id>".
func parseUserMention(s string) (snowflake.ID, bool) {
	if !strings.HasPrefix(s, "<@") || !strings.HasSuffix(s, ">") {
		return 0, false
	}
	inner := strings.TrimPrefix(s[2:len(s)-1], "!")
	id, err := snowflake.Parse(inner)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// parseChannel parses "<#id>" or a bare channel ID.
func parseChannel(s string) (snowflake.ID, bool) {
	if strings.HasPrefix(s, "<#") && strings.HasSuffix(s, ">") {
		s = s[2 : len(s)-1]
	}
	id, err := snowflake.Parse(s)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// channelTarget resolves a channel target to an ID and its mention.
func channelTarget(t target, req Request) (snowflake.ID, string) {
	id := t.channelID
	if id == 0 {
		id = req.ChannelID
	}
	return id, "<#" + id.String() + ">"
}

// guildTarget resolves a guild target to an ID and a name for replies.
func guildTarget(t target, req Request) (snowflake.ID, string) {
	if t.guildID == 0 || t.guildID == req.GuildID {
		return req.GuildID, "This server"
	}
	return t.guildID, fmt.Sprintf("Server `%s`", t.guildID)
}

func reply(ctx context.Context, req Request, format string, a ...any) error {
	// No Mentions: user mentions render as names but never ping.
	return req.Reply.Reply(ctx, Reply{Content: fmt.Sprintf(format, a...)})
}

// Allow is "@Bot allow guild [guildID]" and "@Bot allow @user [command]".
// The guild ID is not checked against the servers the bot is in (no guild cache),
// so an owner can pre-allow a server before inviting the bot.
type Allow struct {
	Access AccessManager
	Known  func(command string) bool // whether a command exists
	// ChannelVisible reports whether the bot can see a channel; nil skips the
	// check. Unknown channels are still allowed (like pre-allowing a guild).
	ChannelVisible func(channelID snowflake.ID) bool
}

func (Allow) Name() string { return "allow" }

func (c Allow) Run(ctx context.Context, req Request) error {
	t, ok := parseTarget(req.Args)
	if !ok {
		return reply(ctx, req, "Usage: `allow guild [serverID]` (defaults to this server), `allow @user [command]` (defaults to `%s`), or `allow ask [#channel or channel ID]` (defaults to this channel).", DefaultGrantCommand)
	}
	if t.channel {
		id, name := channelTarget(t, req)
		changed, err := c.Access.AllowChannel(ctx, id, t.command, req.AuthorID)
		switch {
		case err != nil:
			return reply(ctx, req, saveFailed)
		case !changed:
			return reply(ctx, req, "`%s` was already enabled in %s.", t.command, name)
		}
		msg := fmt.Sprintf("`%s` is now enabled in %s.", t.command, name)
		if c.ChannelVisible != nil && !c.ChannelVisible(id) {
			msg += fmt.Sprintf("\nNote: I can't see channel `%s` (a wrong ID, or I'm not in that server yet). It's saved anyway.", id)
		}
		return reply(ctx, req, "%s", msg)
	}
	if t.guild {
		gid, name := guildTarget(t, req)
		changed, err := c.Access.AllowGuild(ctx, gid, req.AuthorID)
		switch {
		case err != nil:
			return reply(ctx, req, saveFailed)
		case !changed:
			return reply(ctx, req, "%s was already allowed.", name)
		}
		return reply(ctx, req, "%s is now allowed.", name)
	}

	who := Mention(t.user)
	if c.Access.IsOwner(t.user) {
		return reply(ctx, req, "%s is an owner and can already use everything.", who)
	}
	if !c.Known(t.command) {
		return reply(ctx, req, "There's no `%s` command (yet).", t.command)
	}
	changed, err := c.Access.Grant(ctx, t.user, t.command, req.AuthorID)
	switch {
	case errors.Is(err, access.ErrOwnerOnly):
		return reply(ctx, req, "`%s` is owner-only and can't be granted.", t.command)
	case errors.Is(err, access.ErrPublic):
		return reply(ctx, req, "`%s` is already open to everyone in allowed servers.", t.command)
	case errors.Is(err, access.ErrInherited):
		parent, _ := c.Access.InheritsFrom(t.command)
		return reply(ctx, req, "`%s` comes with `%s` access. Grant `%s` instead.", t.command, parent, parent)
	case err != nil:
		return reply(ctx, req, saveFailed)
	case !changed:
		return reply(ctx, req, "%s already had `%s`.", who, t.command)
	}
	msg := fmt.Sprintf("%s can now use `%s` in allowed servers.", who, t.command)
	if allowed, err := c.Access.GuildAllowed(ctx, req.GuildID); err == nil && !allowed {
		msg += "\nNote: this server isn't allowed yet. Use `allow guild` to enable it here."
	}
	return reply(ctx, req, "%s", msg)
}

// Deny is "@Bot deny guild [guildID]" and "@Bot deny @user [command]".
type Deny struct {
	Access AccessManager
}

func (Deny) Name() string { return "deny" }

func (c Deny) Run(ctx context.Context, req Request) error {
	t, ok := parseTarget(req.Args)
	if !ok {
		return reply(ctx, req, "Usage: `deny guild [serverID]` (defaults to this server), `deny @user [command]` (defaults to `%s`), or `deny ask [#channel or channel ID]` (defaults to this channel).", DefaultGrantCommand)
	}
	if t.channel {
		id, name := channelTarget(t, req)
		changed, err := c.Access.DenyChannel(ctx, id, t.command, req.AuthorID)
		switch {
		case err != nil:
			return reply(ctx, req, saveFailed)
		case !changed:
			return reply(ctx, req, "`%s` wasn't enabled in %s.", t.command, name)
		}
		return reply(ctx, req, "`%s` is no longer enabled in %s.", t.command, name)
	}
	if t.guild {
		gid, name := guildTarget(t, req)
		changed, err := c.Access.DenyGuild(ctx, gid, req.AuthorID)
		switch {
		case err != nil:
			return reply(ctx, req, saveFailed)
		case !changed:
			return reply(ctx, req, "%s wasn't allowed.", name)
		}
		return reply(ctx, req, "%s is no longer allowed. I'll only respond to owners there.", name)
	}

	who := Mention(t.user)
	if c.Access.IsOwner(t.user) {
		return reply(ctx, req, "%s is an owner; owners are set in config.yaml, not here.", who)
	}
	// Revoking is not limited to known commands, so stale grants can be cleaned up.
	changed, err := c.Access.Revoke(ctx, t.user, t.command, req.AuthorID)
	switch {
	case err != nil:
		return reply(ctx, req, saveFailed)
	case !changed:
		return reply(ctx, req, "%s didn't have `%s`.", who, t.command)
	}
	return reply(ctx, req, "%s can no longer use `%s`.", who, t.command)
}

// FormatAccess renders a snapshot for Discord. Times use Discord timestamps,
// which each viewer sees in their own timezone.
func FormatAccess(snap access.Snapshot, currentGuild snowflake.ID) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Allowed servers** (%d)\n", len(snap.Guilds))
	if len(snap.Guilds) == 0 {
		b.WriteString("none. Use `allow guild` in a server to allow it.\n")
	}
	for _, g := range snap.Guilds {
		here := ""
		if g.GuildID == currentGuild {
			here = " (this server)"
		}
		fmt.Fprintf(&b, "- `%s`%s, added by %s <t:%d:d>\n", g.GuildID, here, Mention(g.AddedBy), g.AddedAt.Unix())
	}

	// Group grants by user; the snapshot is sorted by user then command.
	var users []snowflake.ID
	cmds := map[snowflake.ID][]string{}
	for _, g := range snap.Grants {
		if _, seen := cmds[g.UserID]; !seen {
			users = append(users, g.UserID)
		}
		cmds[g.UserID] = append(cmds[g.UserID], "`"+g.Command+"`")
	}
	fmt.Fprintf(&b, "**Grants** (%d users)\n", len(users))
	if len(users) == 0 {
		b.WriteString("none. Use `allow @user [command]` to grant one.\n")
	}
	for _, u := range users {
		fmt.Fprintf(&b, "- %s: %s\n", Mention(u), strings.Join(cmds[u], ", "))
	}

	// Only shown once there are any, so bots without ask look as before.
	if len(snap.Channels) > 0 {
		fmt.Fprintf(&b, "**Channels** (%d)\n", len(snap.Channels))
	}
	for _, ch := range snap.Channels {
		fmt.Fprintf(&b, "- `%s` in <#%s> (`%s`), added by %s <t:%d:d>\n", ch.Command, ch.ChannelID, ch.ChannelID, Mention(ch.AddedBy), ch.AddedAt.Unix())
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncate cuts s to at most limit characters, marking the cut.
func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	const marker = "\n… (truncated)"
	runes := []rune(s)
	return string(runes[:limit-utf8.RuneCountInString(marker)]) + marker
}
