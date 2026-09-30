package access

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Checker decides whether a user may run a command in a guild, and then
// whether in this channel. command is the lowercase command name as typed,
// which may not exist.
type Checker interface {
	Allowed(ctx context.Context, userID, guildID snowflake.ID, command string) bool
	// ChannelAllowed reports whether command may run in one of channelIDs (a
	// channel, and a thread's parent). Commands without channel limits run anywhere.
	ChannelAllowed(ctx context.Context, userID snowflake.ID, command string, channelIDs ...snowflake.ID) bool
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

	// Channels where a channel-limited command may run.
	AllowChannel(ctx context.Context, channelID snowflake.ID, command string, e Entry) (changed bool, err error)
	DenyChannel(ctx context.Context, channelID snowflake.ID, command string) (changed bool, err error)
	ChannelAllowed(ctx context.Context, channelID snowflake.ID, command string) (bool, error)

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

// ChannelEntry is one channel where a channel-limited command may run.
type ChannelEntry struct {
	ChannelID snowflake.ID
	Command   string
	Entry
}

// Snapshot is a sorted, read-only copy of the allow-lists.
type Snapshot struct {
	Guilds   []GuildEntry
	Grants   []GrantEntry
	Channels []ChannelEntry
}

// Sort puts a snapshot in the order Backend.Snapshot promises: guilds by ID,
// grants by user ID then command, channels by command then channel ID.
func (s *Snapshot) Sort() {
	slices.SortFunc(s.Guilds, func(a, b GuildEntry) int { return cmp.Compare(a.GuildID, b.GuildID) })
	slices.SortFunc(s.Grants, func(a, b GrantEntry) int {
		return cmp.Or(cmp.Compare(a.UserID, b.UserID), cmp.Compare(a.Command, b.Command))
	})
	slices.SortFunc(s.Channels, func(a, b ChannelEntry) int {
		return cmp.Or(cmp.Compare(a.Command, b.Command), cmp.Compare(a.ChannelID, b.ChannelID))
	})
}
