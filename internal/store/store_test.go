package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
	"github.com/kirkmadraga/vox-engine/internal/cache"
	"github.com/kirkmadraga/vox-engine/internal/cache/cachetest"
)

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tempDB(t *testing.T) string {
	return filepath.Join(t.TempDir(), "data", "bot.db") // "data" does not exist yet
}

// The shared suites run unchanged against SQLite: the same tests the
// in-memory implementations pass.
func TestAccessContract(t *testing.T) {
	accesstest.BackendContract(t, func(t *testing.T) (access.Backend, func(*testing.T) access.Backend) {
		path := tempDB(t)
		first := openAt(t, path)
		return first.Access(), func(t *testing.T) access.Backend {
			first.Close() // simulate a restart
			return openAt(t, path).Access()
		}
	})
}

func TestCacheIndexContract(t *testing.T) {
	cachetest.IndexContract(t, func(t *testing.T) (cache.Index, func(*testing.T) cache.Index) {
		path := tempDB(t)
		first := openAt(t, path)
		return first.CacheIndex(), func(t *testing.T) cache.Index {
			first.Close()
			return openAt(t, path).CacheIndex()
		}
	})
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := tempDB(t)
	db := openAt(t, path)
	var v int
	db.sql.QueryRow("PRAGMA user_version").Scan(&v)
	if v != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, SchemaVersion)
	}
	db.Close()
	openAt(t, path) // reopening must not re-run or fail migrations
}

func TestOpenRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(path); err == nil {
		db.Close()
		t.Fatal("expected an error for a non-database file")
	}
	// The file must be left as it was, not reset.
	if data, _ := os.ReadFile(path); !strings.HasPrefix(string(data), "this is not a database") {
		t.Error("garbage file was modified")
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := tempDB(t)
	db := openAt(t, path)
	db.sql.Exec("PRAGMA user_version = 999")
	db.Close()
	if db, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		if db != nil {
			db.Close()
		}
		t.Fatalf("err = %v, want a newer-schema error", err)
	}
}

func TestAccessAndCacheShareOneFile(t *testing.T) {
	path := tempDB(t)
	db := openAt(t, path)
	ctx := context.Background()
	if _, err := db.Access().AllowGuild(ctx, 1, access.Entry{AddedBy: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.CacheIndex().Put(ctx, cache.Record{ID: "jNQXAC9IVRw", Size: 5}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openAt(t, path)
	if ok, _ := db.Access().GuildAllowed(ctx, 1); !ok {
		t.Error("guild lost")
	}
	if _, ok, _ := db.CacheIndex().Get(ctx, "jNQXAC9IVRw"); !ok {
		t.Error("cache entry lost")
	}
}

// A database from the build before ask channels (schema 1) upgrades in place:
// its guilds and grants survive.
func TestUpgradeFromSchema1KeepsAccessLists(t *testing.T) {
	path := tempDB(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		migrations[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO allowed_guilds (guild_id, added_by, added_at) VALUES (100, 7, '2026-09-29T05:00:00Z')`,
		`INSERT INTO grants (user_id, command, added_by, added_at) VALUES (8, 'play', 7, '2026-09-29T05:00:00Z')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db := openAt(t, path)
	ctx := context.Background()
	snap, err := db.Access().Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Guilds) != 1 || snap.Guilds[0].GuildID != 100 || len(snap.Grants) != 1 || snap.Grants[0].UserID != 8 {
		t.Errorf("access lists after upgrade: %+v", snap)
	}
}

// A v1.1.x database (schema 4) upgrades in place: access lists, reminders and
// balances survive, and open commands work.
func TestUpgradeFromSchema4AddsOpenCommands(t *testing.T) {
	path := tempDB(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{}, migrations[:4]...),
		`PRAGMA user_version = 4`,
		`INSERT INTO allowed_guilds (guild_id, added_by, added_at) VALUES (100, 7, '2026-09-29T05:00:00Z')`,
		`INSERT INTO grants (user_id, command, added_by, added_at) VALUES (8, 'play', 7, '2026-09-29T05:00:00Z')`,
		`INSERT INTO ask_balances (user_id, day, balance) VALUES (8, '2026-10-07', 4)`,
		`INSERT INTO reminders (user_id, guild_id, channel_id, text, next_at, created_at) VALUES (8, 100, 5, 'stretch', 1791379641, '2026-10-07T05:00:00Z')`) {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db := openAt(t, path)
	ctx := context.Background()
	if _, err := db.Access().OpenCommand(ctx, 100, "play", access.Entry{AddedBy: 7}); err != nil {
		t.Fatalf("open after upgrade: %v", err)
	}
	snap, err := db.Access().Snapshot(ctx)
	if err != nil || len(snap.Guilds) != 1 || len(snap.Grants) != 1 || len(snap.Open) != 1 {
		t.Errorf("access lists after upgrade: %+v, %v", snap, err)
	}
	if rs, err := db.AllReminders(ctx); len(rs) != 1 || rs[0].Text != "stretch" || err != nil {
		t.Errorf("reminders after upgrade: %+v, %v", rs, err)
	}
	if _, b, found, err := db.DailyBalance(ctx, 8); !found || b != 4 || err != nil {
		t.Errorf("balance after upgrade: %d %v %v", b, found, err)
	}
}

// A v1.0.0 database with ask channels saved opens and works the same: the
// channel list is ignored, not deleted (an older build would still find it).
func TestSavedAskChannelsAreKeptButIgnored(t *testing.T) {
	path := tempDB(t)
	db := openAt(t, path)
	ctx := context.Background()
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO allowed_channels (channel_id, command, added_by, added_at) VALUES (500, 'ask', 7, '2026-09-30T05:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Access().Grant(ctx, 8, "ask", access.Entry{AddedBy: 7}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db = openAt(t, path)
	snap, err := db.Access().Snapshot(ctx)
	if err != nil || len(snap.Grants) != 1 {
		t.Fatalf("snapshot: %+v, %v", snap, err)
	}
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM allowed_channels`).Scan(&n); err != nil || n != 1 {
		t.Errorf("saved channels: %d, %v; want the row kept", n, err)
	}
}

func TestDailyBalances(t *testing.T) {
	path := tempDB(t)
	db := openAt(t, path)
	ctx := context.Background()
	if _, _, found, err := db.DailyBalance(ctx, 8); found || err != nil {
		t.Fatalf("empty: found=%v err=%v", found, err)
	}
	for _, b := range []int{49, -3} { // a second write replaces the first; debt is allowed
		if err := db.SetDailyBalance(ctx, 8, "2026-09-30", b); err != nil {
			t.Fatal(err)
		}
	}
	db.SetDailyBalance(ctx, 1100000000000000009, "2026-09-29", 12) // large IDs keep precision
	db.Close()

	db = openAt(t, path) // survives a restart
	if day, b, found, err := db.DailyBalance(ctx, 8); !found || err != nil || day != "2026-09-30" || b != -3 {
		t.Errorf("user 8: %q %d %v %v", day, b, found, err)
	}
	if _, b, found, _ := db.DailyBalance(ctx, 1100000000000000009); !found || b != 12 {
		t.Errorf("large ID: %d %v", b, found)
	}
}
