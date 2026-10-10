package store

import (
	"context"
	"fmt"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/remind"
)

var _ remind.Store = (*DB)(nil)

// Due times are Unix seconds, so they sort and compare as numbers.

// AddReminder implements remind.Store.
func (s *DB) AddReminder(ctx context.Context, r remind.Reminder) (int64, error) {
	res, err := s.sql.ExecContext(ctx,
		`INSERT INTO reminders (user_id, user_name, guild_id, channel_id, text, rule, location, next_at, until_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		int64(r.UserID), r.UserName, int64(r.GuildID), int64(r.ChannelID), r.Text, r.Rule.Encode(), r.Location,
		r.Next.Unix(), unixOrZero(r.Until), formatTime(r.Created))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const reminderColumns = `id, user_id, user_name, guild_id, channel_id, text, rule, location, next_at, until_at, created_at`

// unixOrZero is t in Unix seconds, with the zero time (no end) as 0.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// UserReminders implements remind.Store.
func (s *DB) UserReminders(ctx context.Context, user snowflake.ID) ([]remind.Reminder, error) {
	return s.reminders(ctx, `SELECT `+reminderColumns+` FROM reminders WHERE user_id = ? ORDER BY next_at, id`, int64(user))
}

// AllReminders implements remind.Store.
func (s *DB) AllReminders(ctx context.Context) ([]remind.Reminder, error) {
	return s.reminders(ctx, `SELECT `+reminderColumns+` FROM reminders ORDER BY next_at, id`)
}

func (s *DB) reminders(ctx context.Context, query string, args ...any) ([]remind.Reminder, error) {
	rows, err := s.sql.QueryContext(ctx, query, args...)
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

// ReminderCounts implements remind.Store.
func (s *DB) ReminderCounts(ctx context.Context) (total, repeating int, err error) {
	err = s.sql.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(NULLIF(rule, '')) FROM reminders`).Scan(&total, &repeating)
	return total, repeating, err
}

func scanReminder(row scanner) (remind.Reminder, error) {
	var r remind.Reminder
	var user, guild, channel, next, until int64
	var rule, created string
	if err := row.Scan(&r.ID, &user, &r.UserName, &guild, &channel, &r.Text, &rule, &r.Location, &next, &until, &created); err != nil {
		return r, err
	}
	r.UserID, r.GuildID, r.ChannelID = snowflake.ID(user), snowflake.ID(guild), snowflake.ID(channel)
	r.Next = time.Unix(next, 0).UTC()
	if until != 0 {
		r.Until = time.Unix(until, 0).UTC()
	}
	var err error
	if r.Rule, err = remind.DecodeRule(rule); err != nil {
		return r, fmt.Errorf("reminder %d: %w", r.ID, err)
	}
	if r.Created, err = parseTime(created); err != nil {
		return r, fmt.Errorf("reminder %d: %w", r.ID, err)
	}
	return r, nil
}
