package access

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Errors returned by Policy.Grant.
var (
	ErrOwnerOnly = errors.New("command is owner-only and cannot be granted")
	ErrPublic    = errors.New("command is already open to everyone in allowed guilds")
	ErrInherited = errors.New("command shares another command's access and cannot be granted on its own")
)

// Options configures a Policy.
type Options struct {
	Owners    []snowflake.ID
	Public    []string // commands anyone may run in allowed guilds
	OwnerOnly []string // commands that can never be granted
	// Inherit maps a command to the command whose access it shares, e.g.
	// "skip" -> "play": whoever may run play may run skip.
	Inherit map[string]string
	Now     func() time.Time
	Logger  *slog.Logger
}

// Policy applies the access rules on top of a Backend:
//
//   - Owners may run anything, in any guild. Owner status comes from config only.
//   - Anyone else needs the guild to be allowed, and then either a public command
//     or a personal grant for it. Grants are global per user.
//   - Owner-only commands are never allowed for non-owners and cannot be granted.
//   - Inheriting commands (e.g. skip -> play) are checked as their parent and
//     cannot be granted themselves.
type Policy struct {
	backend   Backend
	owners    map[snowflake.ID]struct{}
	public    map[string]struct{}
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
		public:    lowerSet(opts.Public),
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

// IsPublic reports whether command is open to everyone in allowed guilds.
func (p *Policy) IsPublic(command string) bool {
	_, ok := p.public[strings.ToLower(command)]
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
	if p.IsOwner(userID) {
		return true
	}
	command = strings.ToLower(command)
	if parent, ok := p.inherit[command]; ok {
		command = parent
	}
	if p.IsOwnerOnly(command) {
		return false
	}
	ok, err := p.backend.GuildAllowed(ctx, guildID)
	if err != nil {
		p.logger.Error("access check failed", "guild", guildID, "err", err)
		return false
	}
	if !ok {
		return false
	}
	if p.IsPublic(command) {
		return true
	}
	ok, err = p.backend.HasGrant(ctx, userID, command)
	if err != nil {
		p.logger.Error("access check failed", "user", userID, "command", command, "err", err)
		return false
	}
	return ok
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

// Grant lets userID run command in allowed guilds.
func (p *Policy) Grant(ctx context.Context, userID snowflake.ID, command string, by snowflake.ID) (bool, error) {
	command = strings.ToLower(command)
	switch {
	case p.IsOwnerOnly(command):
		return false, ErrOwnerOnly
	case p.IsPublic(command):
		return false, ErrPublic
	}
	if _, ok := p.inherit[command]; ok {
		return false, ErrInherited
	}
	changed, err := p.backend.Grant(ctx, userID, command, p.entry(by))
	return changed, p.logChange(changed, err, "grant", "user", userID, "command", command, "by", by)
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
