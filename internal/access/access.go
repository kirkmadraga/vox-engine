package access

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Checker decides whether a user may run a command in a guild. command is
// the lowercase command name as typed, which may not exist.
type Checker interface {
	Allowed(ctx context.Context, userID, guildID snowflake.ID, command string) bool
}

// Backend stores the allow-lists. It knows nothing about owners or rules;
// Policy layers those on top. Implementations: store.Access (SQLite) in
// production, accesstest.Memory in tests. Every implementation must pass
// accesstest.BackendContract.
//
// Mutations report changed=false when the state already matched (no-op).
// Command names arrive lowercased. Implementations must be safe for concurrent use.
type Backend interface {
	AllowGuild(ctx context.Context, guildID snowflake.ID, e Entry) (changed bool, err error)
	DenyGuild(ctx context.Context, guildID snowflake.ID) (changed bool, err error)
	GuildAllowed(ctx context.Context, guildID snowflake.ID) (bool, error)

	Grant(ctx context.Context, userID snowflake.ID, command string, e Entry) (changed bool, err error)
	Revoke(ctx context.Context, userID snowflake.ID, command string) (changed bool, err error)
	HasGrant(ctx context.Context, userID snowflake.ID, command string) (bool, error)

	// Open commands: anyone may run command in guildID, or anywhere when
	// guildID is Public.
	OpenCommand(ctx context.Context, guildID snowflake.ID, command string, e Entry) (changed bool, err error)
	CloseCommand(ctx context.Context, guildID snowflake.ID, command string) (changed bool, err error)
	// CommandOpen reports whether command is open in exactly guildID (Public
	// is checked on its own).
	CommandOpen(ctx context.Context, guildID snowflake.ID, command string) (bool, error)

	// Snapshot returns everything, sorted as Snapshot.Sort does.
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Entry records who added an allow-list entry and when.
type Entry struct {
	AddedBy snowflake.ID
	AddedAt time.Time
}

// GuildEntry is one allowed guild in a Snapshot.
type GuildEntry struct {
	GuildID snowflake.ID
	Entry
}

// GrantEntry is one user's permission to run one command.
type GrantEntry struct {
	UserID  snowflake.ID
	Command string
	Entry
}

// Public, as an open command's guild, means every server, allowed or not.
// Discord never uses 0 as an ID.
const Public snowflake.ID = 0

// OpenEntry is one command opened to everyone in a guild, or everywhere when
// GuildID is Public.
type OpenEntry struct {
	GuildID snowflake.ID
	Command string
	Entry
}

// Snapshot is a sorted, read-only copy of the allow-lists.
type Snapshot struct {
	Guilds []GuildEntry
	Grants []GrantEntry
	Open   []OpenEntry
}

// Sort puts a snapshot in the order Backend.Snapshot promises: guilds by ID,
// grants by user ID then command, open commands by guild ID (Public first)
// then command.
func (s *Snapshot) Sort() {
	slices.SortFunc(s.Guilds, func(a, b GuildEntry) int { return cmp.Compare(a.GuildID, b.GuildID) })
	slices.SortFunc(s.Grants, func(a, b GrantEntry) int {
		return cmp.Or(cmp.Compare(a.UserID, b.UserID), cmp.Compare(a.Command, b.Command))
	})
	slices.SortFunc(s.Open, func(a, b OpenEntry) int {
		return cmp.Or(cmp.Compare(a.GuildID, b.GuildID), cmp.Compare(a.Command, b.Command))
	})
}
