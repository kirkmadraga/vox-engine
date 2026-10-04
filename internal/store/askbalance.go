package store

import (
	"context"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
)

var _ commands.DailyBalances = (*DB)(nil)

// DailyBalance implements commands.DailyBalances.
func (s *DB) DailyBalance(ctx context.Context, user snowflake.ID) (day string, balance int, found bool, err error) {
	err = s.sql.QueryRowContext(ctx, `SELECT day, balance FROM ask_balances WHERE user_id = ?`, int64(user)).Scan(&day, &balance)
	if isNoRows(err) {
		return "", 0, false, nil
	}
	return day, balance, err == nil, err
}

// AllDailyBalances implements commands.DailyBalances.
func (s *DB) AllDailyBalances(ctx context.Context) ([]commands.DailyRow, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT user_id, day, balance FROM ask_balances`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commands.DailyRow
	for rows.Next() {
		var user int64
		var r commands.DailyRow
		if err := rows.Scan(&user, &r.Day, &r.Balance); err != nil {
			return nil, err
		}
		r.User = snowflake.ID(user)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetDailyBalance implements commands.DailyBalances.
func (s *DB) SetDailyBalance(ctx context.Context, user snowflake.ID, day string, balance int) error {
	_, err := s.sql.ExecContext(ctx,
		`INSERT INTO ask_balances (user_id, day, balance) VALUES (?, ?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET day = excluded.day, balance = excluded.balance`,
		int64(user), day, balance)
	return err
}
