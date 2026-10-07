package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/remind"
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
	Open(ctx context.Context, guildID snowflake.ID, command string, by snowflake.ID) (bool, error)
	Close(ctx context.Context, guildID snowflake.ID, command string, by snowflake.ID) (bool, error)
	Check(ctx context.Context, userID, guildID snowflake.ID, command string) (bool, error)
	Snapshot(ctx context.Context) (access.Snapshot, error)
}

const saveFailed = "Couldn't save that change, so nothing changed. Details are in the bot's log."

// target is what "allow"/"deny" act on: a guild (guildID 0 means the current
// one), a user and a command, or a command for everyone in this guild
// (everyone) or in every guild (public).
type target struct {
	guild    bool
	guildID  snowflake.ID
	user     snowflake.ID
	everyone bool
	public   bool
	command  string
}

// parseTarget parses "guild [guildID]", "<@id> [command]", "everyone
// [command]" or "public [command]".
func parseTarget(args string) (target, bool) {
	fields := strings.Fields(args)
	if len(fields) == 0 || len(fields) > 2 {
		return target{}, false
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
	t := target{command: DefaultGrantCommand}
	switch {
	case strings.EqualFold(fields[0], "everyone"):
		t.everyone = true
	case strings.EqualFold(fields[0], "public"):
		t.public = true
	default:
		user, ok := parseUserMention(fields[0])
		if !ok {
			return target{}, false
		}
		t.user = user
	}
	if len(fields) == 2 {
		t.command = strings.ToLower(fields[1])
	}
	return t, true
}

// opening resolves an everyone or public target to the guild it's stored
// under and who it reaches, for replies.
func opening(t target, req Request) (snowflake.ID, string) {
	if t.public {
		return access.Public, "everyone, in every server I'm in"
	}
	return req.GuildID, "everyone in this server"
}

// refusal explains why command can't be granted or opened ("" for other
// errors). done is "granted" or "opened"; instead is "Grant" or "Open".
func refusal(am AccessManager, err error, command, done, instead string) string {
	switch {
	case errors.Is(err, access.ErrOwnerOnly):
		return fmt.Sprintf("`%s` is owner-only and can't be %s.", command, done)
	case errors.Is(err, access.ErrAlwaysOpen):
		return fmt.Sprintf("`%s` is always open to everyone in allowed servers.", command)
	case errors.Is(err, access.ErrInherited):
		parent, _ := am.InheritsFrom(command)
		return fmt.Sprintf("`%s` comes with `%s` access. %s `%s` instead.", command, parent, instead, parent)
	}
	return ""
}

const (
	allowUsage = "Usage: `allow guild [serverID]` (defaults to this server), `allow @user [command]`, " +
		"`allow everyone [command]` (everyone in this server) or `allow public [command]` (everyone, in every server); " +
		"the command defaults to `%s`."
	denyUsage = "Usage: `deny guild [serverID]` (defaults to this server), `deny @user [command]`, " +
		"`deny everyone [command]` or `deny public [command]`; the command defaults to `%s`."
)

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

// Allow is "@Bot allow guild [guildID]", "@Bot allow @user [command]",
// "@Bot allow everyone [command]" and "@Bot allow public [command]".
// The guild ID is not checked against the servers the bot is in (no guild cache),
// so an owner can pre-allow a server before inviting the bot.
type Allow struct {
	Access AccessManager
	Known  func(command string) bool // whether a command exists
}

func (Allow) Name() string { return "allow" }

func (c Allow) Run(ctx context.Context, req Request) error {
	t, ok := parseTarget(req.Args)
	if !ok {
		return reply(ctx, req, allowUsage, DefaultGrantCommand)
	}
	if t.everyone || t.public {
		return c.open(ctx, req, t)
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
	if why := refusal(c.Access, err, t.command, "granted", "Grant"); why != "" {
		return reply(ctx, req, "%s", why)
	}
	switch {
	case err != nil:
		return reply(ctx, req, saveFailed)
	case !changed:
		return reply(ctx, req, "%s already had `%s`.", who, t.command)
	}
	msg := fmt.Sprintf("%s can now use `%s` in allowed servers.", who, t.command)
	return reply(ctx, req, "%s%s", msg, c.notAllowedNote(ctx, req))
}

// open is "allow everyone/public [command]".
func (c Allow) open(ctx context.Context, req Request, t target) error {
	gid, whom := opening(t, req)
	if !c.Known(t.command) {
		return reply(ctx, req, "There's no `%s` command (yet).", t.command)
	}
	changed, err := c.Access.Open(ctx, gid, t.command, req.AuthorID)
	if errors.Is(err, access.ErrAlwaysOpen) { // only "everyone" gets here: public is accepted
		return reply(ctx, req, "`%s` is always open to everyone in allowed servers. Use `allow public %s` for every server.", t.command, t.command)
	}
	if why := refusal(c.Access, err, t.command, "opened", "Open"); why != "" {
		return reply(ctx, req, "%s", why)
	}
	switch {
	case err != nil:
		return reply(ctx, req, saveFailed)
	case !changed:
		return reply(ctx, req, "`%s` was already open to %s.", t.command, whom)
	}
	note := ""
	if t.everyone {
		note = c.notAllowedNote(ctx, req)
	} else {
		note = publicWarning(t.command)
	}
	return reply(ctx, req, "`%s` is now open to %s.%s", t.command, whom, note)
}

// publicWarning is the reply's note on opening command publicly: strangers
// can add the bot (Discord's "Public Bot" is on by default) and use it.
func publicWarning(command string) string {
	cost := ""
	if command == AskCommand {
		cost = ", each with their own daily allowance, all on your API key"
	}
	return "\nAnyone who can add me to a server can use it there" + cost + ". To keep it to your own servers, " +
		"turn off **Public Bot** in the Developer Portal (your app → Bot), so only you can add me."
}

// notAllowedNote warns when this server isn't allowed, so a grant or an
// opening doesn't work here yet.
func (c Allow) notAllowedNote(ctx context.Context, req Request) string {
	if allowed, err := c.Access.GuildAllowed(ctx, req.GuildID); err == nil && !allowed {
		return "\nNote: this server isn't allowed yet. Use `allow guild` to enable it here."
	}
	return ""
}

// Deny is "@Bot deny guild [guildID]", "@Bot deny @user [command]",
// "@Bot deny everyone [command]" and "@Bot deny public [command]".
// Whenever that can take remindme away from someone, the reminders whose
// owner can no longer use remindme where they set them are deleted; the rest
// (including owners') are kept.
type Deny struct {
	Access    AccessManager
	Reminders ReminderPruner // nil = no reminders to clean up
}

// ReminderPruner lets Deny find and delete reminders that lost their access.
// remind.Store implements it.
type ReminderPruner interface {
	AllReminders(ctx context.Context) ([]remind.Reminder, error)
	DeleteReminder(ctx context.Context, id int64) (bool, error)
}

func (Deny) Name() string { return "deny" }

func (c Deny) Run(ctx context.Context, req Request) error {
	t, ok := parseTarget(req.Args)
	if !ok {
		return reply(ctx, req, denyUsage, DefaultGrantCommand)
	}
	if t.everyone || t.public {
		gid, whom := opening(t, req)
		changed, err := c.Access.Close(ctx, gid, t.command, req.AuthorID)
		switch {
		case err != nil:
			return reply(ctx, req, saveFailed)
		case !changed:
			return reply(ctx, req, "`%s` wasn't open to %s.", t.command, whom)
		}
		return reply(ctx, req, "`%s` is no longer open to %s.%s", t.command, whom, c.pruneIf(ctx, t.command))
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
		return reply(ctx, req, "%s is no longer allowed. Only owners and public commands work there now.%s%s",
			name, c.keptOpenings(ctx, gid), c.pruneIf(ctx, RemindCommand))
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
	return reply(ctx, req, "%s can no longer use `%s`.%s", who, t.command, c.pruneIf(ctx, t.command))
}

// keptOpenings mentions the commands opened to everyone in guild, which a
// denied guild keeps (inactive) and gets back if it's allowed again.
func (c Deny) keptOpenings(ctx context.Context, guild snowflake.ID) string {
	snap, err := c.Access.Snapshot(ctx)
	if err != nil {
		return ""
	}
	var cmds []string
	for _, o := range snap.Open {
		if o.GuildID == guild {
			cmds = append(cmds, "`"+o.Command+"`")
		}
	}
	switch len(cmds) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("\nIts open command (%s) comes back if you allow it again.", cmds[0])
	}
	return fmt.Sprintf("\nIts %d open commands (%s) come back if you allow it again.", len(cmds), strings.Join(cmds, ", "))
}

// pruneIf deletes the reminders that lost their access, when command (what
// was taken away) is remindme, and says how many went.
func (c Deny) pruneIf(ctx context.Context, command string) string {
	if command != RemindCommand || c.Reminders == nil {
		return ""
	}
	n, err := c.prune(ctx)
	switch {
	case err != nil:
		return "\nCouldn't check all the reminders that went with it; any left will be dropped when due."
	case n == 1:
		return "\n1 reminder that went with it was deleted."
	case n > 1:
		return fmt.Sprintf("\n%d reminders that went with it were deleted.", n)
	}
	return ""
}

// prune deletes every reminder whose owner can no longer use remindme in the
// server it was set in. It stops at the first storage error, so a failing
// access check never reads as "no access".
func (c Deny) prune(ctx context.Context) (int, error) {
	rs, err := c.Reminders.AllReminders(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		ok, err := c.Access.Check(ctx, r.UserID, r.GuildID, RemindCommand)
		if err != nil {
			return n, err
		}
		if ok {
			continue
		}
		deleted, err := c.Reminders.DeleteReminder(ctx, r.ID)
		if err != nil {
			return n, err
		}
		if deleted {
			n++
		}
	}
	return n, nil
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

	// Group open commands by guild; sorted by guild (Public first) then command.
	var guilds []snowflake.ID
	open := map[snowflake.ID][]string{}
	for _, o := range snap.Open {
		if _, seen := open[o.GuildID]; !seen {
			guilds = append(guilds, o.GuildID)
		}
		open[o.GuildID] = append(open[o.GuildID], "`"+o.Command+"`")
	}
	fmt.Fprintf(&b, "**Open to everyone** (%d)\n", len(snap.Open))
	if len(snap.Open) == 0 {
		b.WriteString("none. Use `allow everyone [command]` in a server, or `allow public [command]` for every server.\n")
	}
	allowed := map[snowflake.ID]bool{}
	for _, g := range snap.Guilds {
		allowed[g.GuildID] = true
	}
	for _, g := range guilds {
		if g == access.Public {
			fmt.Fprintf(&b, "- every server (public): %s\n", strings.Join(open[g], ", "))
			continue
		}
		var notes []string
		if g == currentGuild {
			notes = append(notes, "this server")
		}
		if !allowed[g] {
			notes = append(notes, "not allowed: inactive")
		}
		where := fmt.Sprintf("`%s`", g)
		if len(notes) > 0 {
			where += " (" + strings.Join(notes, ", ") + ")"
		}
		fmt.Fprintf(&b, "- %s: %s\n", where, strings.Join(open[g], ", "))
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
