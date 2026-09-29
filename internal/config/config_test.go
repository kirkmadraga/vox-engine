package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
  - "1100000000000000011"
  - "1100000000000000012"
cache_dir: ./audio
max_concurrent_jobs: 2
max_queue_length: 25
search_results: 7
`
	cfg, err := Parse([]byte(yaml), env(fakeToken))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []snowflake.ID{1100000000000000011, 1100000000000000012}
	if len(cfg.OwnerIDs) != 2 || cfg.OwnerIDs[0] != want[0] || cfg.OwnerIDs[1] != want[1] {
		t.Errorf("OwnerIDs = %v, want %v", cfg.OwnerIDs, want)
	}
	if cfg.Token != fakeToken || cfg.CacheDir != "./audio" || cfg.MaxConcurrentJobs != 2 || cfg.MaxQueueLength != 25 || cfg.SearchResults != 7 {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
	if cfg.Database != "data/bot.db" || cfg.CacheMaxBytes != 512<<20 || cfg.CacheMaxAge != 12*time.Hour {
		t.Errorf("defaults: database=%q max_bytes=%d max_age=%v", cfg.Database, cfg.CacheMaxBytes, cfg.CacheMaxAge)
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
			cfg, err := Parse([]byte(yaml), env(fakeToken))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(cfg.OwnerIDs) != 0 {
				t.Errorf("OwnerIDs = %v, want empty", cfg.OwnerIDs)
			}
			if cfg.MaxConcurrentJobs != 1 || cfg.CacheDir != "cache" || cfg.MaxQueueLength != DefaultMaxQueueLength || cfg.SearchResults != DefaultSearchResults {
				t.Errorf("defaults not applied: %+v", cfg)
			}
		})
	}
}

func TestParseUnquotedIDKeepsPrecision(t *testing.T) {
	// A large unquoted ID must not be rounded through float64.
	cfg, err := Parse([]byte("owner_user_ids:\n  - 123456789012345678\n"), env(fakeToken))
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
		"missing token":      {"", "", "DISCORD_TOKEN"},
		"blank token":        {"", "   ", "DISCORD_TOKEN"},
		"malformed id":       {"owner_user_ids: [\"abc\"]\n", fakeToken, "owner_user_ids[0]"},
		"zero id":            {"owner_user_ids: [\"0\"]\n", fakeToken, "owner_user_ids[0]"},
		"unknown key":        {"owner_ids: []\n", fakeToken, "owner_ids"},
		"bad jobs":           {"max_concurrent_jobs: 0\n", fakeToken, "max_concurrent_jobs"},
		"bad queue length":   {"max_queue_length: 0\n", fakeToken, "max_queue_length"},
		"zero results":       {"search_results: 0\n", fakeToken, "search_results"},
		"too many results":   {"search_results: 11\n", fakeToken, "search_results"},
		"negative cache cap": {"cache_max_bytes: -1\n", fakeToken, "cache_max_bytes"},
		"bad cache age":      {"cache_max_age: soon\n", fakeToken, "cache_max_age"},
		"negative cache age": {"cache_max_age: -1h\n", fakeToken, "cache_max_age"},
		"old state_file key": {"state_file: ./data/access.json\n", fakeToken, "state_file"},
		"negative duration":  {"max_duration_seconds: -5\n", fakeToken, "max_duration_seconds"},
		"invalid yaml":       {"owner_user_ids: [\n", fakeToken, "parse config"},
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
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), env(fakeToken))
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

func TestParseCacheSettings(t *testing.T) {
	cases := map[string]struct {
		yaml     string
		db       string
		maxBytes int64
		maxAge   time.Duration
	}{
		"explicit": {"database: ./x/y.db\ncache_max_bytes: 536870912\ncache_max_age: 12h\n", "./x/y.db", 536870912, 12 * time.Hour},
		"disabled": {"cache_max_bytes: 0\ncache_max_age: \"0\"\n", "data/bot.db", 0, 0},
		"minutes":  {"cache_max_age: 90m\n", "data/bot.db", 512 << 20, 90 * time.Minute},
	}
	for name, c := range cases {
		cfg, err := Parse([]byte(c.yaml), env(fakeToken))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cfg.Database != c.db || cfg.CacheMaxBytes != c.maxBytes || cfg.CacheMaxAge != c.maxAge {
			t.Errorf("%s: got %q %d %v", name, cfg.Database, cfg.CacheMaxBytes, cfg.CacheMaxAge)
		}
	}
}

func TestParseTestTone(t *testing.T) {
	cfg, _ := Parse([]byte(""), env(fakeToken))
	if cfg.TestTone != "assets/tone.opus" {
		t.Errorf("default test_tone = %q", cfg.TestTone)
	}
	cfg, _ = Parse([]byte("test_tone: /opt/bot/tone.opus\n"), env(fakeToken))
	if cfg.TestTone != "/opt/bot/tone.opus" {
		t.Errorf("test_tone = %q", cfg.TestTone)
	}
}

// The committed tone must exist where the default points (tests run in this
// package's directory, so go up to the repository root).
func TestDefaultTestToneIsCommitted(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", DefaultTestTone)); err != nil {
		t.Errorf("default test tone missing: %v", err)
	}
}

func TestParseCookies(t *testing.T) {
	cfg, err := Parse([]byte("ytdlp_cookies: \" ./data/cookies.txt \"\n"), env(fakeToken))
	if err != nil || cfg.YtdlpCookies != "./data/cookies.txt" {
		t.Errorf("ytdlp_cookies = %q, %v", cfg.YtdlpCookies, err)
	}
	if cfg, _ := Parse([]byte(""), env(fakeToken)); cfg.YtdlpCookies != "" {
		t.Errorf("default = %q, want none", cfg.YtdlpCookies)
	}
}

func TestParseYtdlpMinInterval(t *testing.T) {
	for yaml, want := range map[string]time.Duration{
		"":                            30 * time.Second, // default
		"ytdlp_min_interval: 45s\n":   45 * time.Second,
		"ytdlp_min_interval: \"0\"\n": 0,
	} {
		cfg, err := Parse([]byte(yaml), env(fakeToken))
		if err != nil || cfg.YtdlpMinInterval != want {
			t.Errorf("%q: got %v, %v; want %v", yaml, cfg.YtdlpMinInterval, err, want)
		}
	}
	for _, bad := range []string{"ytdlp_min_interval: soon\n", "ytdlp_min_interval: -5s\n"} {
		if _, err := Parse([]byte(bad), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "ytdlp_min_interval") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

// fakeToken has the shape of a real bot token (base64 ID . timestamp . HMAC)
// without being one.
var fakeToken = base64.RawStdEncoding.EncodeToString([]byte("1100000000000000001")) + ".GxYz12.fake-hmac-part-for-tests"

func TestTokenShape(t *testing.T) {
	good := []string{
		fakeToken,
		base64.StdEncoding.EncodeToString([]byte("1100000000000000001")) + ".a.b", // padded
	}
	for _, tok := range good {
		if _, err := Parse(nil, env(tok)); err != nil {
			t.Errorf("token %q rejected: %v", tok, err)
		}
	}
	bad := []string{
		"tok",
		"Bot " + fakeToken, // prefix pasted along
		"abc.def",          // two parts
		"!!!.def.ghi",      // not base64
		base64.RawStdEncoding.EncodeToString([]byte("hello")) + ".a.b", // not a numeric ID
		fakeToken + ".extra",
	}
	for _, tok := range bad {
		_, err := Parse(nil, env(tok))
		if err == nil || !strings.Contains(err.Error(), "doesn't look like a bot token") {
			t.Errorf("token %q: err = %v", tok, err)
		}
	}
}

func TestExampleOwnerIDsRejected(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(example, env(fakeToken))
	if err == nil || !strings.Contains(err.Error(), "still the example value") {
		t.Errorf("unedited example config: err = %v", err)
	}
	if _, err := Parse([]byte("owner_user_ids: [\"222222222222222222\"]\n"), env(fakeToken)); err == nil {
		t.Error("second example ID should be rejected too")
	}
}

func TestParseVoiceAndSearchLimits(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxVoiceSessions != 1 || cfg.VoiceIdleTimeout != 0 || cfg.MaxConcurrentSearches != 1 {
		t.Errorf("defaults: sessions=%d idle=%v searches=%d", cfg.MaxVoiceSessions, cfg.VoiceIdleTimeout, cfg.MaxConcurrentSearches)
	}
	cfg, err = Parse([]byte("max_voice_sessions: 4\nvoice_idle_timeout: 30s\nmax_concurrent_searches: 3\n"), env(fakeToken))
	if err != nil || cfg.MaxVoiceSessions != 4 || cfg.VoiceIdleTimeout != 30*time.Second || cfg.MaxConcurrentSearches != 3 {
		t.Errorf("custom: %+v, %v", cfg, err)
	}
	cfg, err = Parse([]byte("voice_idle_timeout: \"0\"\n"), env(fakeToken))
	if err != nil || cfg.VoiceIdleTimeout != 0 {
		t.Errorf("idle 0: %v, %v", cfg.VoiceIdleTimeout, err)
	}
	for yaml, want := range map[string]string{
		"max_voice_sessions: 0\n":      "max_voice_sessions",
		"max_voice_sessions: -2\n":     "max_voice_sessions",
		"max_concurrent_searches: 0\n": "max_concurrent_searches",
		"voice_idle_timeout: -1m\n":    "voice_idle_timeout",
		"voice_idle_timeout: soon\n":   "voice_idle_timeout",
	} {
		if _, err := Parse([]byte(yaml), env(fakeToken)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want mention of %s", yaml, err, want)
		}
	}
}
