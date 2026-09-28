package jsonfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bot/internal/access"
	"bot/internal/access/accesstest"
)

func TestContract(t *testing.T) {
	accesstest.BackendContract(t, func(t *testing.T) (access.Backend, func(*testing.T) access.Backend) {
		path := filepath.Join(t.TempDir(), "data", "access.json") // "data" does not exist yet
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s, func(t *testing.T) access.Backend {
			r, err := Open(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			return r
		}
	})
}

var entry1 = access.Entry{AddedBy: 1100000000000000001, AddedAt: time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)}

func TestFileFormatIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	s, _ := Open(path)
	ctx := context.Background()
	if _, err := s.AllowGuild(ctx, 1100000000000000003, entry1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, 1100000000000000002, "play", entry1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"version": 1`,
		`"1100000000000000003": {`,
		`"1100000000000000002": {`,
		`"play": {`,
		`"added_by": "1100000000000000001"`,
		`"added_at": "2026-09-29T05:00:00Z"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file missing %s:\n%s", want, data)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("expected only access.json, got %d entries (temp file left behind?)", len(entries))
	}
}

func TestOpenCorruptFileFails(t *testing.T) {
	for name, content := range map[string]string{
		"not json":      "{nope",
		"empty file":    "",
		"wrong version": `{"version": 2, "guilds": {}, "grants": {}}`,
		"unknown field": `{"version": 1, "guilds": {}, "grants": {}, "roles": {}}`,
		"bad id key":    `{"version": 1, "guilds": {"abc": {}}, "grants": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "access.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Error("expected error for corrupt state file")
			}
		})
	}
}

func TestOpenNullMapsTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	if err := os.WriteFile(path, []byte(`{"version": 1, "guilds": null, "grants": null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(context.Background(), 1, "play", entry1); err != nil {
		t.Errorf("grant after null maps: %v", err)
	}
}

func TestFailedWriteLeavesMemoryUnchanged(t *testing.T) {
	// A directory at the state path makes the final rename fail.
	path := filepath.Join(t.TempDir(), "access.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Store{path: path, st: emptyFile()}
	ctx := context.Background()
	changed, err := s.AllowGuild(ctx, 100, entry1)
	if err == nil || changed {
		t.Fatalf("changed=%v err=%v, want write error", changed, err)
	}
	if ok, _ := s.GuildAllowed(ctx, 100); ok {
		t.Error("memory changed despite failed write")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
