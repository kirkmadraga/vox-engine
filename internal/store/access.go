package store

import (
	"context"
	"fmt"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
)

// Access is the SQLite access.Backend. Discord IDs are stored as INTEGER:
// snowflakes are below 2^63, so they fit int64 exactly.
type Access struct{ db *DB }

var _ access.Backend = Access{}

// Access returns the access backend over this database.
func (s *DB) Access() Access { return Access{db: s} }

func (a Access) AllowGuild(ctx context.Context, guildID snowflake.ID, e access.Entry) (bool, error) {
	return changed(a.db.sql.ExecContext(ctx,
		`INSERT INTO allowed_guilds (guild_id, added_by, added_at) VALUES (?, ?, ?)
		 ON CONFLICT (guild_id) DO NOTHING`,
		int64(guildID), int64(e.AddedBy), formatTime(e.AddedAt)))
}

func (a Access) DenyGuild(ctx context.Context, guildID snowflake.ID) (bool, error) {
	return changed(a.db.sql.ExecContext(ctx, `DELETE FROM allowed_guilds WHERE guild_id = ?`, int64(guildID)))
}

func (a Access) GuildAllowed(ctx context.Context, guildID snowflake.ID) (bool, error) {
	return a.exists(ctx, `SELECT 1 FROM allowed_guilds WHERE guild_id = ?`, int64(guildID))
}

func (a Access) Grant(ctx context.Context, userID snowflake.ID, command string, e access.Entry) (bool, error) {
	return changed(a.db.sql.ExecContext(ctx,
		`INSERT INTO grants (user_id, command, added_by, added_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (user_id, command) DO NOTHING`,
		int64(userID), command, int64(e.AddedBy), formatTime(e.AddedAt)))
}

func (a Access) Revoke(ctx context.Context, userID snowflake.ID, command string) (bool, error) {
	return changed(a.db.sql.ExecContext(ctx, `DELETE FROM grants WHERE user_id = ? AND command = ?`, int64(userID), command))
}

func (a Access) HasGrant(ctx context.Context, userID snowflake.ID, command string) (bool, error) {
	return a.exists(ctx, `SELECT 1 FROM grants WHERE user_id = ? AND command = ?`, int64(userID), command)
}

func (a Access) AllowChannel(ctx context.Context, channelID snowflake.ID, command string, e access.Entry) (bool, error) {
	return changed(a.db.sql.ExecContext(ctx,
		`INSERT INTO allowed_channels (channel_id, command, added_by, added_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (channel_id, command) DO NOTHING`,
		int64(channelID), command, int64(e.AddedBy), formatTime(e.AddedAt)))
}

func (a Access) DenyChannel(ctx context.Context, channelID snowflake.ID, command string) (bool, error) {
	return changed(a.db.sql.ExecContext(ctx, `DELETE FROM allowed_channels WHERE channel_id = ? AND command = ?`, int64(channelID), command))
}

func (a Access) ChannelAllowed(ctx context.Context, channelID snowflake.ID, command string) (bool, error) {
	return a.exists(ctx, `SELECT 1 FROM allowed_channels WHERE channel_id = ? AND command = ?`, int64(channelID), command)
}

func (a Access) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var one int
	err := a.db.sql.QueryRowContext(ctx, query, args...).Scan(&one)
	if isNoRows(err) {
		return false, nil
	}
	return err == nil, err
}

func (a Access) Snapshot(ctx context.Context) (access.Snapshot, error) {
	var snap access.Snapshot

	rows, err := a.db.sql.QueryContext(ctx, `SELECT guild_id, added_by, added_at FROM allowed_guilds`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var guild, by int64
		var at string
		if err := rows.Scan(&guild, &by, &at); err != nil {
			rows.Close()
			return snap, err
		}
		t, err := parseTime(at)
		if err != nil {
			rows.Close()
			return snap, fmt.Errorf("allowed_guilds %d: %w", guild, err)
		}
		snap.Guilds = append(snap.Guilds, access.GuildEntry{GuildID: snowflake.ID(guild), Entry: access.Entry{AddedBy: snowflake.ID(by), AddedAt: t}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, err
	}
	if err := rows.Close(); err != nil {
		return snap, err
	}

	// Grants and channels share a shape: (ID, command, added_by, added_at).
	err = a.idCommandRows(ctx, "grants", `SELECT user_id, command, added_by, added_at FROM grants`,
		func(id snowflake.ID, cmd string, e access.Entry) {
			snap.Grants = append(snap.Grants, access.GrantEntry{UserID: id, Command: cmd, Entry: e})
		})
	if err != nil {
		return snap, err
	}
	err = a.idCommandRows(ctx, "allowed_channels", `SELECT channel_id, command, added_by, added_at FROM allowed_channels`,
		func(id snowflake.ID, cmd string, e access.Entry) {
			snap.Channels = append(snap.Channels, access.ChannelEntry{ChannelID: id, Command: cmd, Entry: e})
		})
	if err != nil {
		return snap, err
	}
	snap.Sort() // Go-side sort matches the other backends exactly
	return snap, nil
}

func (a Access) idCommandRows(ctx context.Context, table, query string, add func(snowflake.ID, string, access.Entry)) error {
	rows, err := a.db.sql.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, by int64
		var cmd, at string
		if err := rows.Scan(&id, &cmd, &by, &at); err != nil {
			return err
		}
		t, err := parseTime(at)
		if err != nil {
			return fmt.Errorf("%s %d/%s: %w", table, id, cmd, err)
		}
		add(snowflake.ID(id), cmd, access.Entry{AddedBy: snowflake.ID(by), AddedAt: t})
	}
	return rows.Err()
}
