package access

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Errors returned by Policy.Grant and Policy.Open.
var (
	ErrOwnerOnly  = errors.New("command is owner-only and cannot be granted")
	ErrAlwaysOpen = errors.New("command is always open to everyone in allowed guilds")
	ErrInherited  = errors.New("command shares another command's access and cannot be granted on its own")
)

// Options configures a Policy.
type Options struct {
	Owners     []snowflake.ID
	AlwaysOpen []string // commands anyone may always run in allowed guilds (ping)
	OwnerOnly  []string // commands that can never be granted or opened
	// Inherit maps a command to the command whose access it shares, e.g.
	// "skip" -> "play": whoever may run play may run skip.
	Inherit map[string]string
	Now     func() time.Time
	Logger  *slog.Logger
}

// Policy applies the access rules on top of a Backend. A command is allowed
// at the first of these that lets the user in, widest first:
//
//  1. Owners may run anything, in any guild. Owner status comes from config only.
//  2. A public command (opened with Guild Public) may be run by anyone in any
//     guild, allowed or not.
//  3. In an allowed guild: an always-open command (ping), or one opened to
//     everyone in that guild.
//  4. In an allowed guild: a personal grant. Grants are global per user.
//
// Each level only adds access, so there's no per-user block: denying someone
// a grant doesn't stop them using a command opened to everyone.
// Owner-only commands are never allowed for non-owners and can't be granted
// or opened. Inheriting commands (e.g. skip -> play) are checked as their
// parent and can't be granted or opened themselves.
//
// There are deliberately no per-channel rules (the bot operator's choice):
// every command follows the same guild and grant rules in every channel.
// Commands that cost money (ask) are capped by their own limits instead, and
// server admins can still keep the bot out of a channel with Discord's own
// permissions.
type Policy struct {
	backend   Backend
	owners    map[snowflake.ID]struct{}
	always    map[string]struct{}
	ownerOnly map[string]struct{}
	inherit   map[string]string
	now       func() time.Time
	logger    *slog.Logger
}

// NewPolicy builds a Policy over backend.
func NewPolicy(backend Backend, opts Options) *Policy {
	p := &Policy{
		backend:   backend,
		owners:    make(map[snowflake.ID]struct{}, len(opts.Owners)),
		always:    lowerSet(opts.AlwaysOpen),
		ownerOnly: lowerSet(opts.OwnerOnly),
		inherit:   make(map[string]string, len(opts.Inherit)),
		now:       opts.Now,
		logger:    opts.Logger,
	}
	for _, id := range opts.Owners {
		p.owners[id] = struct{}{}
	}
	for child, parent := range opts.Inherit {
		p.inherit[strings.ToLower(child)] = strings.ToLower(parent)
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.logger == nil {
		p.logger = slog.New(slog.DiscardHandler)
	}
	return p
}

func lowerSet(names []string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = struct{}{}
	}
	return m
}

// IsOwner reports whether userID is a configured owner.
func (p *Policy) IsOwner(userID snowflake.ID) bool {
	_, ok := p.owners[userID]
	return ok
}

// IsAlwaysOpen reports whether command is always open to everyone in allowed
// guilds.
func (p *Policy) IsAlwaysOpen(command string) bool {
	_, ok := p.always[strings.ToLower(command)]
	return ok
}

// IsOwnerOnly reports whether command can never be granted.
func (p *Policy) IsOwnerOnly(command string) bool {
	_, ok := p.ownerOnly[strings.ToLower(command)]
	return ok
}

// InheritsFrom returns the command whose access command shares, if any.
func (p *Policy) InheritsFrom(command string) (string, bool) {
	parent, ok := p.inherit[strings.ToLower(command)]
	return parent, ok
}

// Allowed implements Checker. Backend errors deny (and are logged): failing
// closed is safer than answering strangers.
func (p *Policy) Allowed(ctx context.Context, userID, guildID snowflake.ID, command string) bool {
	ok, err := p.Check(ctx, userID, guildID, command)
	if err != nil {
		p.logger.Error("access check failed", "user", userID, "guild", guildID, "command", command, "err", err)
		return false
	}
	return ok
}

// Check is Allowed with the backend's error returned instead of logged, for
// callers that must not mistake a storage failure for "no access" (e.g.
// before deleting something).
func (p *Policy) Check(ctx context.Context, userID, guildID snowflake.ID, command string) (bool, error) {
	if p.IsOwner(userID) {
		return true, nil
	}
	command = strings.ToLower(command)
	if parent, ok := p.inherit[command]; ok {
		command = parent
	}
	if p.IsOwnerOnly(command) {
		return false, nil
	}
	if ok, err := p.backend.CommandOpen(ctx, Public, command); err != nil || ok {
		return ok, err
	}
	if ok, err := p.backend.GuildAllowed(ctx, guildID); err != nil || !ok {
		return false, err
	}
	if p.IsAlwaysOpen(command) {
		return true, nil
	}
	if ok, err := p.backend.CommandOpen(ctx, guildID, command); err != nil || ok {
		return ok, err
	}
	return p.backend.HasGrant(ctx, userID, command)
}

// GuildAllowed reports whether guildID is on the allow-list.
func (p *Policy) GuildAllowed(ctx context.Context, guildID snowflake.ID) (bool, error) {
	return p.backend.GuildAllowed(ctx, guildID)
}

// AllowGuild adds guildID to the allow-list.
func (p *Policy) AllowGuild(ctx context.Context, guildID, by snowflake.ID) (bool, error) {
	changed, err := p.backend.AllowGuild(ctx, guildID, p.entry(by))
	return changed, p.logChange(changed, err, "allow guild", "guild", guildID, "by", by)
}

// DenyGuild removes guildID from the allow-list. User grants are kept.
func (p *Policy) DenyGuild(ctx context.Context, guildID, by snowflake.ID) (bool, error) {
	changed, err := p.backend.DenyGuild(ctx, guildID)
	return changed, p.logChange(changed, err, "deny guild", "guild", guildID, "by", by)
}

// grantable reports why command (lowercase) can't be granted or opened, if it can't.
func (p *Policy) grantable(command string) error {
	switch {
	case p.IsOwnerOnly(command):
		return ErrOwnerOnly
	case p.IsAlwaysOpen(command):
		return ErrAlwaysOpen
	}
	if _, ok := p.inherit[command]; ok {
		return ErrInherited
	}
	return nil
}

// Grant lets userID run command in allowed guilds.
func (p *Policy) Grant(ctx context.Context, userID snowflake.ID, command string, by snowflake.ID) (bool, error) {
	command = strings.ToLower(command)
	if err := p.grantable(command); err != nil {
		return false, err
	}
	changed, err := p.backend.Grant(ctx, userID, command, p.entry(by))
	return changed, p.logChange(changed, err, "grant", "user", userID, "command", command, "by", by)
}

// Open lets anyone run command in guildID (if it's allowed), or in every
// guild when guildID is Public. It refuses the same commands Grant does,
// except that an always-open command can be made public (it then works in
// guilds that aren't allowed too).
func (p *Policy) Open(ctx context.Context, guildID snowflake.ID, command string, by snowflake.ID) (bool, error) {
	command = strings.ToLower(command)
	if err := p.grantable(command); err != nil && !(errors.Is(err, ErrAlwaysOpen) && guildID == Public) {
		return false, err
	}
	changed, err := p.backend.OpenCommand(ctx, guildID, command, p.entry(by))
	return changed, p.logChange(changed, err, "open", "guild", guildID, "command", command, "by", by)
}

// Close undoes Open. Like Revoke, it takes any name, so stale entries can be
// cleaned up.
func (p *Policy) Close(ctx context.Context, guildID snowflake.ID, command string, by snowflake.ID) (bool, error) {
	command = strings.ToLower(command)
	changed, err := p.backend.CloseCommand(ctx, guildID, command)
	return changed, p.logChange(changed, err, "close", "guild", guildID, "command", command, "by", by)
}

// Revoke removes userID's grant for command.
func (p *Policy) Revoke(ctx context.Context, userID snowflake.ID, command string, by snowflake.ID) (bool, error) {
	command = strings.ToLower(command)
	changed, err := p.backend.Revoke(ctx, userID, command)
	return changed, p.logChange(changed, err, "revoke", "user", userID, "command", command, "by", by)
}

// Snapshot returns the current allow-lists.
func (p *Policy) Snapshot(ctx context.Context) (Snapshot, error) {
	return p.backend.Snapshot(ctx)
}

func (p *Policy) entry(by snowflake.ID) Entry {
	return Entry{AddedBy: by, AddedAt: p.now().UTC()}
}

func (p *Policy) logChange(changed bool, err error, action string, attrs ...any) error {
	switch {
	case err != nil:
		p.logger.Error("access change failed", append([]any{"action", action, "err", err}, attrs...)...)
	case changed:
		p.logger.Info("access changed", append([]any{"action", action}, attrs...)...)
	}
	return err
}
