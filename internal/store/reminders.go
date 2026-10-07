package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/remind"
)

var _ remind.Store = (*DB)(nil)

// Due times are Unix seconds, so they sort and compare as numbers.

// AddReminder implements remind.Store.
func (s *DB) AddReminder(ctx context.Context, r remind.Reminder) (int64, error) {
	res, err := s.sql.ExecContext(ctx,
		`INSERT INTO reminders (user_id, user_name, guild_id, channel_id, text, rule, location, next_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		int64(r.UserID), r.UserName, int64(r.GuildID), int64(r.ChannelID), r.Text, r.Rule.Encode(), r.Location,
		r.Next.Unix(), formatTime(r.Created))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const reminderColumns = `id, user_id, user_name, guild_id, channel_id, text, rule, location, next_at, created_at`

// UserReminders implements remind.Store.
func (s *DB) UserReminders(ctx context.Context, user snowflake.ID) ([]remind.Reminder, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT `+reminderColumns+` FROM reminders WHERE user_id = ? ORDER BY next_at, id`, int64(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []remind.Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SoonestReminder implements remind.Store.
func (s *DB) SoonestReminder(ctx context.Context) (remind.Reminder, bool, error) {
	r, err := scanReminder(s.sql.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM reminders ORDER BY next_at, id LIMIT 1`))
	if isNoRows(err) {
		return remind.Reminder{}, false, nil
	}
	return r, err == nil, err
}

// RescheduleReminder implements remind.Store.
func (s *DB) RescheduleReminder(ctx context.Context, id int64, next time.Time) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE reminders SET next_at = ? WHERE id = ?`, next.Unix(), id)
	return err
}

// DeleteReminder implements remind.Store.
func (s *DB) DeleteReminder(ctx context.Context, id int64) (bool, error) {
	return changed(s.sql.ExecContext(ctx, `DELETE FROM reminders WHERE id = ?`, id))
}

// DeleteUserReminders implements remind.Store.
func (s *DB) DeleteUserReminders(ctx context.Context, user snowflake.ID) (int, error) {
	return deleted(s.sql.ExecContext(ctx, `DELETE FROM reminders WHERE user_id = ?`, int64(user)))
}

// DeleteGuildReminders implements remind.Store.
func (s *DB) DeleteGuildReminders(ctx context.Context, guild snowflake.ID, keep []snowflake.ID) (int, error) {
	query := `DELETE FROM reminders WHERE guild_id = ?`
	args := []any{int64(guild)}
	if len(keep) > 0 {
		query += ` AND user_id NOT IN (?` + strings.Repeat(`, ?`, len(keep)-1) + `)`
		for _, id := range keep {
			args = append(args, int64(id))
		}
	}
	return deleted(s.sql.ExecContext(ctx, query, args...))
}

// ReminderCounts implements remind.Store.
func (s *DB) ReminderCounts(ctx context.Context) (total, repeating int, err error) {
	err = s.sql.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(NULLIF(rule, '')) FROM reminders`).Scan(&total, &repeating)
	return total, repeating, err
}

func scanReminder(row scanner) (remind.Reminder, error) {
	var r remind.Reminder
	var user, guild, channel, next int64
	var rule, created string
	if err := row.Scan(&r.ID, &user, &r.UserName, &guild, &channel, &r.Text, &rule, &r.Location, &next, &created); err != nil {
		return r, err
	}
	r.UserID, r.GuildID, r.ChannelID = snowflake.ID(user), snowflake.ID(guild), snowflake.ID(channel)
	r.Next = time.Unix(next, 0).UTC()
	var err error
	if r.Rule, err = remind.DecodeRule(rule); err != nil {
		return r, fmt.Errorf("reminder %d: %w", r.ID, err)
	}
	if r.Created, err = parseTime(created); err != nil {
		return r, fmt.Errorf("reminder %d: %w", r.ID, err)
	}
	return r, nil
}

func deleted(res interface{ RowsAffected() (int64, error) }, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
