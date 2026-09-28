package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bot/internal/access"
	"bot/internal/access/accesstest"
	"bot/internal/cache"
	"bot/internal/cache/cachetest"
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
