// Package store keeps the bot's persistent state in one SQLite database:
// the access allow-lists and the audio cache index. It uses modernc.org/sqlite,
// a pure-Go driver, so builds stay CGO_ENABLED=0 and run on baseline x86-64.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// migrations[i] upgrades the schema from version i to i+1. Append only; never
// edit a released migration.
var migrations = []string{
	// 1: access allow-lists and the cache index.
	`CREATE TABLE allowed_guilds (
		guild_id INTEGER PRIMARY KEY,
		added_by INTEGER NOT NULL,
		added_at TEXT    NOT NULL
	);
	CREATE TABLE grants (
		user_id  INTEGER NOT NULL,
		command  TEXT    NOT NULL,
		added_by INTEGER NOT NULL,
		added_at TEXT    NOT NULL,
		PRIMARY KEY (user_id, command)
	);
	CREATE TABLE cache_entries (
		video_id    TEXT    PRIMARY KEY,
		title       TEXT    NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		size_bytes  INTEGER NOT NULL,
		added_at    TEXT    NOT NULL,
		last_played TEXT,
		play_count  INTEGER NOT NULL DEFAULT 0
	);`,
	// 2: channels where a channel-limited command (ask) may run. Unused since
	// v1.1.0 removed channel limits; kept, not dropped, so nothing is deleted
	// and an older build still finds its list.
	`CREATE TABLE allowed_channels (
		channel_id INTEGER NOT NULL,
		command    TEXT    NOT NULL,
		added_by   INTEGER NOT NULL,
		added_at   TEXT    NOT NULL,
		PRIMARY KEY (channel_id, command)
	);`,
	// 3: each user's daily ask balance (a count only, never content).
	`CREATE TABLE ask_balances (
		user_id INTEGER PRIMARY KEY,
		day     TEXT    NOT NULL,
		balance INTEGER NOT NULL
	);`,
	// 4: reminders (remindme). next_at is Unix seconds, so it sorts; text is
	// the user's own words, sent back to them and never logged.
	`CREATE TABLE reminders (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL,
		user_name  TEXT    NOT NULL DEFAULT '',
		guild_id   INTEGER NOT NULL,
		channel_id INTEGER NOT NULL,
		text       TEXT    NOT NULL,
		rule       TEXT    NOT NULL DEFAULT '',
		location   TEXT    NOT NULL DEFAULT '',
		next_at    INTEGER NOT NULL,
		created_at TEXT    NOT NULL
	);
	CREATE INDEX reminders_next ON reminders (next_at);
	CREATE INDEX reminders_user ON reminders (user_id);`,
	// 5: commands opened to everyone in a guild ("allow everyone"), or in
	// every guild when guild_id is 0 ("allow public").
	`CREATE TABLE open_commands (
		guild_id INTEGER NOT NULL,
		command  TEXT    NOT NULL,
		added_by INTEGER NOT NULL,
		added_at TEXT    NOT NULL,
		PRIMARY KEY (guild_id, command)
	);`,
	// 6: a repeating reminder's end ("until", "for"), Unix seconds; 0 = none.
	`ALTER TABLE reminders ADD COLUMN until_at INTEGER NOT NULL DEFAULT 0;`,
}

// SchemaVersion is the version this build writes.
var SchemaVersion = len(migrations)

// DB is the open database.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the database at path, checks it, and
// migrates it to SchemaVersion. A corrupt file or one written by a newer
// build is an error, never silently reset.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database dir: %w", err)
		}
	}
	q := url.Values{}
	for _, p := range []string{
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		"synchronous(FULL)", // durability over speed: writes are rare
		"foreign_keys(1)",
	} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection: SQLite serialises writes anyway, and this avoids
	// "database is locked" between our own connections.
	db.SetMaxOpenConns(1)

	s := &DB{sql: db}
	if err := s.check(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *DB) Close() error { return s.sql.Close() }

func (s *DB) check() error {
	var result string
	if err := s.sql.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("unreadable (fix or remove it): %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity check failed (fix or remove it): %s", result)
	}
	return nil
}

func (s *DB) migrate() error {
	var version int
	if err := s.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("schema version %d is newer than this build supports (%d)", version, SchemaVersion)
	}
	for v := version; v < SchemaVersion; v++ {
		tx, err := s.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate to version %d: %w", v+1, err)
		}
		// PRAGMA can't take a bound parameter; v is an int we control.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate to version %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate to version %d: %w", v+1, err)
		}
	}
	return nil
}

// Times are stored as RFC 3339 text in UTC with nanoseconds, so they sort
// correctly as text and round-trip exactly.
const timeFormat = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeFormat, s) }

func parseNullTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	return parseTime(ns.String)
}

// changed reports whether a statement affected any row.
func changed(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
