package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/cache"
)

// CacheIndex is the SQLite cache.Index.
type CacheIndex struct{ db *DB }

var _ cache.Index = CacheIndex{}

// CacheIndex returns the cache index over this database.
func (s *DB) CacheIndex() CacheIndex { return CacheIndex{db: s} }

func (c CacheIndex) Put(ctx context.Context, r cache.Record) error {
	_, err := c.db.sql.ExecContext(ctx,
		`INSERT INTO cache_entries (video_id, title, duration_ms, size_bytes, added_at, last_played, play_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (video_id) DO UPDATE SET
		   title = excluded.title, duration_ms = excluded.duration_ms, size_bytes = excluded.size_bytes,
		   added_at = excluded.added_at, last_played = excluded.last_played, play_count = excluded.play_count`,
		r.ID, r.Title, r.Duration.Milliseconds(), r.Size, formatTime(r.AddedAt), nullTime(r.LastPlayed), r.PlayCount)
	return err
}

func (c CacheIndex) Get(ctx context.Context, id string) (cache.Record, bool, error) {
	row := c.db.sql.QueryRowContext(ctx, selectEntries+` WHERE video_id = ?`, id)
	r, err := scanEntry(row)
	if isNoRows(err) {
		return cache.Record{}, false, nil
	}
	return r, err == nil, err
}

func (c CacheIndex) MarkPlayed(ctx context.Context, id string, at time.Time) error {
	_, err := c.db.sql.ExecContext(ctx,
		`UPDATE cache_entries SET last_played = ?, play_count = play_count + 1 WHERE video_id = ?`,
		formatTime(at), id)
	return err
}

func (c CacheIndex) Delete(ctx context.Context, id string) error {
	_, err := c.db.sql.ExecContext(ctx, `DELETE FROM cache_entries WHERE video_id = ?`, id)
	return err
}

func (c CacheIndex) All(ctx context.Context) ([]cache.Record, error) {
	rows, err := c.db.sql.QueryContext(ctx, selectEntries+` ORDER BY video_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cache.Record
	for rows.Next() {
		r, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const selectEntries = `SELECT video_id, title, duration_ms, size_bytes, added_at, last_played, play_count FROM cache_entries`

type scanner interface{ Scan(dest ...any) error }

func scanEntry(s scanner) (cache.Record, error) {
	var r cache.Record
	var durMs int64
	var added string
	var played sql.NullString
	if err := s.Scan(&r.ID, &r.Title, &durMs, &r.Size, &added, &played, &r.PlayCount); err != nil {
		return cache.Record{}, err
	}
	var err error
	if r.AddedAt, err = parseTime(added); err != nil {
		return cache.Record{}, fmt.Errorf("cache_entries %s: %w", r.ID, err)
	}
	if r.LastPlayed, err = parseNullTime(played); err != nil {
		return cache.Record{}, fmt.Errorf("cache_entries %s: %w", r.ID, err)
	}
	r.Duration = time.Duration(durMs) * time.Millisecond
	return r, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}
