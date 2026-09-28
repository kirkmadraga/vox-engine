package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func env(token string) func(string) string {
	return func(k string) string {
		if k == "DISCORD_TOKEN" {
			return token
		}
		return ""
	}
}

func TestParseValid(t *testing.T) {
	yaml := `
owner_user_ids:
  - "111111111111111111"
  - "222222222222222222"
cache_dir: ./audio
max_concurrent_jobs: 2
max_queue_length: 25
`
	cfg, err := Parse([]byte(yaml), env("tok"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []snowflake.ID{111111111111111111, 222222222222222222}
	if len(cfg.OwnerIDs) != 2 || cfg.OwnerIDs[0] != want[0] || cfg.OwnerIDs[1] != want[1] {
		t.Errorf("OwnerIDs = %v, want %v", cfg.OwnerIDs, want)
	}
	if cfg.Token != "tok" || cfg.CacheDir != "./audio" || cfg.MaxConcurrentJobs != 2 || cfg.MaxQueueLength != 25 {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
	if cfg.StateFile != "data/access.json" {
		t.Errorf("StateFile default = %q", cfg.StateFile)
	}
}

func TestParseOwnerListsEmptyOrMissing(t *testing.T) {
	for name, yaml := range map[string]string{
		"missing key": "max_duration_seconds: 0\n",
		"null key":    "owner_user_ids:\n",
		"empty list":  "owner_user_ids: []\n",
		"empty file":  "",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(yaml), env("tok"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(cfg.OwnerIDs) != 0 {
				t.Errorf("OwnerIDs = %v, want empty", cfg.OwnerIDs)
			}
			if cfg.MaxConcurrentJobs != 1 || cfg.CacheDir != "cache" || cfg.MaxQueueLength != DefaultMaxQueueLength {
				t.Errorf("defaults not applied: %+v", cfg)
			}
		})
	}
}

func TestParseUnquotedIDKeepsPrecision(t *testing.T) {
	// A large unquoted ID must not be rounded through float64.
	cfg, err := Parse([]byte("owner_user_ids:\n  - 123456789012345678\n"), env("tok"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.OwnerIDs[0] != 123456789012345678 {
		t.Errorf("got %d", cfg.OwnerIDs[0])
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		yaml, token, wantErr string
	}{
		"missing token":     {"", "", "DISCORD_TOKEN"},
		"blank token":       {"", "   ", "DISCORD_TOKEN"},
		"malformed id":      {"owner_user_ids: [\"abc\"]\n", "tok", "owner_user_ids[0]"},
		"zero id":           {"owner_user_ids: [\"0\"]\n", "tok", "owner_user_ids[0]"},
		"unknown key":       {"owner_ids: []\n", "tok", "owner_ids"},
		"bad jobs":          {"max_concurrent_jobs: 0\n", "tok", "max_concurrent_jobs"},
		"bad queue length":  {"max_queue_length: 0\n", "tok", "max_queue_length"},
		"negative duration": {"max_duration_seconds: -5\n", "tok", "max_duration_seconds"},
		"invalid yaml":      {"owner_user_ids: [\n", "tok", "parse config"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml), env(c.token))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), env("tok"))
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	if err := LoadDotEnv(filepath.Join(dir, "missing.env")); err != nil {
		t.Errorf("missing .env should be ignored, got %v", err)
	}

	p := filepath.Join(dir, "test.env")
	if err := os.WriteFile(p, []byte("VOXTEST_DOTENV_KEY=from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOXTEST_DOTENV_KEY", "") // registers cleanup; empty but set is not overridden
	os.Unsetenv("VOXTEST_DOTENV_KEY")
	if err := LoadDotEnv(p); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv("VOXTEST_DOTENV_KEY"); got != "from-file" {
		t.Errorf("got %q, want from-file", got)
	}
}
