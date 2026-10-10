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

func TestParseUnauthorizedMessage(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil || cfg.UnauthorizedMessage != DefaultUnauthorizedMessage {
		t.Errorf("default = %q, %v", cfg.UnauthorizedMessage, err)
	}
	cfg, err = Parse([]byte("unauthorized_message: \"  Ask the owner for access.  \"\n"), env(fakeToken))
	if err != nil || cfg.UnauthorizedMessage != "Ask the owner for access." {
		t.Errorf("custom = %q, %v", cfg.UnauthorizedMessage, err)
	}
	for yaml, want := range map[string]string{
		"unauthorized_message: \"\"\n":                              "can't be empty",
		"unauthorized_message: \"   \"\n":                           "can't be empty",
		"unauthorized_message: " + strings.Repeat("x", 2001) + "\n": "longer than 2000",
	} {
		if _, err := Parse([]byte(yaml), env(fakeToken)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestParseLLMProvider(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil || cfg.LLMProvider != "" {
		t.Errorf("default = %q, %v; want off", cfg.LLMProvider, err)
	}
	cfg, err = Parse([]byte("llm_provider: \" Echo \"\nllm_system_prompt: \"\"\n"), env(fakeToken))
	if err != nil || cfg.LLMProvider != "echo" {
		t.Errorf("echo = %q, %v", cfg.LLMProvider, err)
	}
	if _, err := Parse([]byte("llm_provider: gpt\n"), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "llm_provider") {
		t.Errorf("unknown provider: err = %v", err)
	}
}

func TestParseLLMLimits(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil || cfg.LLMUserCooldown != 10*time.Second || cfg.LLMMaxConcurrent != 3 {
		t.Errorf("defaults = %v, %d, %v; want 10s, 3", cfg.LLMUserCooldown, cfg.LLMMaxConcurrent, err)
	}
	cfg, err = Parse([]byte("llm_user_cooldown: 0\nllm_max_concurrent: 1\n"), env(fakeToken))
	if err != nil || cfg.LLMUserCooldown != 0 || cfg.LLMMaxConcurrent != 1 {
		t.Errorf("custom = %v, %d, %v", cfg.LLMUserCooldown, cfg.LLMMaxConcurrent, err)
	}
	for _, yaml := range []string{"llm_user_cooldown: -1s\n", "llm_user_cooldown: soon\n", "llm_max_concurrent: 0\n"} {
		if _, err := Parse([]byte(yaml), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "llm_") {
			t.Errorf("%q: err = %v", yaml, err)
		}
	}
}

func TestParseLLMHistory(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil || cfg.LLMHistoryMessages != 10 || cfg.LLMHistoryMaxAge != 10*time.Minute {
		t.Errorf("defaults = %d, %v, %v; want 10, 10m", cfg.LLMHistoryMessages, cfg.LLMHistoryMaxAge, err)
	}
	cfg, err = Parse([]byte("llm_history_messages: 0\nllm_history_max_age: 0\n"), env(fakeToken))
	if err != nil || cfg.LLMHistoryMessages != 0 || cfg.LLMHistoryMaxAge != 0 {
		t.Errorf("off = %d, %v, %v", cfg.LLMHistoryMessages, cfg.LLMHistoryMaxAge, err)
	}
	for _, yaml := range []string{"llm_history_messages: -1\n", "llm_history_max_age: -1m\n", "llm_history_max_age: long\n"} {
		if _, err := Parse([]byte(yaml), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "llm_history") {
			t.Errorf("%q: err = %v", yaml, err)
		}
	}
}

func TestParseLLMGuardrails(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMTimeout != 60*time.Second || cfg.LLMMaxPromptChars != 500 || cfg.LLMDailyLimit != 50 ||
		cfg.LLMDailyLimitOwners || cfg.LLMDailyResetLocation != time.UTC || cfg.LLMWeightSearch != 8 || cfg.LLMWeightImage != 4 {
		t.Errorf("defaults: %v %d %d %v %v %d %d", cfg.LLMTimeout, cfg.LLMMaxPromptChars, cfg.LLMDailyLimit,
			cfg.LLMDailyLimitOwners, cfg.LLMDailyResetLocation, cfg.LLMWeightSearch, cfg.LLMWeightImage)
	}
	yaml := "llm_timeout: 90s\nllm_max_prompt_chars: 0\nllm_daily_limit: 0\nllm_daily_limit_owners: true\n" +
		"llm_daily_reset_timezone: Asia/Tokyo\nllm_weight_search: 3\nllm_weight_image: 5\n"
	cfg, err = Parse([]byte(yaml), env(fakeToken))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMTimeout != 90*time.Second || cfg.LLMMaxPromptChars != 0 || cfg.LLMDailyLimit != 0 || !cfg.LLMDailyLimitOwners ||
		cfg.LLMDailyResetLocation.String() != "Asia/Tokyo" || cfg.LLMWeightSearch != 3 || cfg.LLMWeightImage != 5 {
		t.Errorf("custom values not applied: %+v", cfg)
	}
	// Midnight in Tokyo is 15:00 UTC the day before.
	if got := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC).In(cfg.LLMDailyResetLocation).Format("2006-01-02 15:04"); got != "2026-10-01 00:00" {
		t.Errorf("Tokyo time: %s", got)
	}
	for yaml, want := range map[string]string{
		"llm_timeout: 0\n":                         "llm_timeout",
		"llm_timeout: soon\n":                      "llm_timeout",
		"llm_max_prompt_chars: -1\n":               "llm_max_prompt_chars",
		"llm_daily_limit: -5\n":                    "llm_daily_limit",
		"llm_weight_image: -1\n":                   "llm_weight_image",
		"llm_daily_reset_timezone: Mars/Olympus\n": "isn't a known timezone",
	} {
		if _, err := Parse([]byte(yaml), env(fakeToken)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", yaml, err, want)
		}
	}
}

// reminder_timezone defaults to ask's reset timezone, which defaults to UTC.
func TestParseReminderTimezone(t *testing.T) {
	for yaml, want := range map[string]string{
		"":                                       "UTC",
		"llm_daily_reset_timezone: Asia/Tokyo\n": "Asia/Tokyo",
		"llm_daily_reset_timezone: Asia/Tokyo\nreminder_timezone: Europe/Berlin\n": "Europe/Berlin",
	} {
		cfg, err := Parse([]byte(yaml), env(fakeToken))
		if err != nil || cfg.ReminderLocation.String() != want {
			t.Errorf("%q: %v, %v; want %s", yaml, cfg.ReminderLocation, err, want)
		}
	}
	if _, err := Parse([]byte("reminder_timezone: Mars/Olympus\n"), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "reminder_timezone") {
		t.Errorf("bad timezone: %v", err)
	}
}

func TestParseDiscordMessageContent(t *testing.T) {
	if cfg, err := Parse([]byte(""), env(fakeToken)); err != nil || cfg.DiscordMessageContent {
		t.Errorf("default must be off: %v, %v", cfg.DiscordMessageContent, err)
	}
	if cfg, err := Parse([]byte("discord_message_content: true\n"), env(fakeToken)); err != nil || !cfg.DiscordMessageContent {
		t.Errorf("true: %v, %v", cfg.DiscordMessageContent, err)
	}
}

// envWith is env plus LLM_API_KEY.
func envWith(token, key string) func(string) string {
	return func(k string) string {
		switch k {
		case "DISCORD_TOKEN":
			return token
		case "LLM_API_KEY":
			return key
		}
		return ""
	}
}

// The system prompt must be written down whenever the bridge is on; "" means none.
func TestParseLLMSystemPromptRequired(t *testing.T) {
	if _, err := Parse([]byte("llm_provider: echo\n"), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "llm_system_prompt is required") {
		t.Errorf("missing prompt: err = %v", err)
	}
	cfg, err := Parse([]byte("llm_provider: echo\nllm_system_prompt: \"\"\n"), env(fakeToken))
	if err != nil || cfg.LLMSystemPrompt != "" {
		t.Errorf("empty prompt: %q, %v", cfg.LLMSystemPrompt, err)
	}
	cfg, err = Parse([]byte("llm_provider: echo\nllm_system_prompt: |\n  You are a bot.\n  Be brief.\n"), env(fakeToken))
	if err != nil || cfg.LLMSystemPrompt != "You are a bot.\nBe brief." {
		t.Errorf("block prompt: %q, %v", cfg.LLMSystemPrompt, err)
	}
	if _, err := Parse([]byte("llm_system_prompt: hi\n"), env(fakeToken)); err != nil {
		t.Errorf("a prompt with the bridge off is fine: %v", err)
	}
}

func TestParseLLMBridgeXAI(t *testing.T) {
	base := "llm_provider: xai\nllm_system_prompt: \"\"\n"
	if _, err := Parse([]byte(base), envWith(fakeToken, "k")); err == nil || !strings.Contains(err.Error(), "llm_model") {
		t.Errorf("missing model: err = %v", err)
	}
	if _, err := Parse([]byte(base+"llm_model: grok-4.3\n"), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("missing key: err = %v", err)
	}
	cfg, err := Parse([]byte(base+"llm_model: grok-4.3\nllm_web_search: On-Request\nllm_web_search_images: true\n"), envWith(fakeToken, " xai-secret "))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMBaseURL != "https://api.x.ai/v1" || cfg.LLMModel != "grok-4.3" || cfg.LLMAPIKey != "xai-secret" ||
		cfg.LLMMaxTokens != 500 || cfg.LLMMaxToolTurns != 2 || cfg.LLMWebSearch != "on-request" || !cfg.LLMWebSearchImages || cfg.LLMReasoningEffort != "" {
		t.Errorf("xai config: %+v", cfg)
	}
	cfg, err = Parse([]byte("llm_provider: openai\nllm_system_prompt: \"\"\nllm_model: m\nllm_base_url: http://localhost:8080/v1/\n"+
		"llm_max_tokens: 800\nllm_max_tool_turns: 0\nllm_reasoning_effort: Low\n"), envWith(fakeToken, "k"))
	if err != nil || cfg.LLMBaseURL != "http://localhost:8080/v1" || cfg.LLMMaxTokens != 800 || cfg.LLMMaxToolTurns != 0 || cfg.LLMReasoningEffort != "low" {
		t.Errorf("openai overrides: %+v, %v", cfg, err)
	}
	if cfg.LLMReminderModel != "" || cfg.LLMReminderReasoningEffort != "" {
		t.Errorf("reminder model: unset by default, got %q %q", cfg.LLMReminderModel, cfg.LLMReminderReasoningEffort)
	}
	cfg, err = Parse([]byte(base+"llm_model: grok-4.7\nllm_reasoning_effort: none\n"+
		"llm_reminder_model: \" grok-4.3 \"\nllm_reminder_reasoning_effort: None\n"), envWith(fakeToken, "k"))
	if err != nil || cfg.LLMReminderModel != "grok-4.3" || cfg.LLMReminderReasoningEffort != "none" || cfg.LLMReasoningEffort != "none" {
		t.Errorf("reminder model: %q %q (ask's effort %q), %v", cfg.LLMReminderModel, cfg.LLMReminderReasoningEffort, cfg.LLMReasoningEffort, err)
	}
	for yaml, want := range map[string]string{
		"llm_max_tokens: 0\n":                  "llm_max_tokens",
		"llm_max_tool_turns: -1\n":             "llm_max_tool_turns",
		"llm_reasoning_effort: max\n":          "llm_reasoning_effort",
		"llm_reminder_reasoning_effort: off\n": "llm_reminder_reasoning_effort",
		"llm_base_url: ftp://x\n":              "llm_base_url",
	} {
		if _, err := Parse([]byte(base+"llm_model: m\n"+yaml), envWith(fakeToken, "k")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", yaml, err, want)
		}
	}
}

// The key must never show up in errors.
func TestParseLLMErrorsNeverShowTheKey(t *testing.T) {
	_, err := Parse([]byte("llm_provider: xai\nllm_system_prompt: \"\"\nllm_model: m\nllm_max_tokens: 0\n"), envWith(fakeToken, "xai-SECRET-123"))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}

func TestParseLLMWebSearchModes(t *testing.T) {
	cfg, err := Parse([]byte(""), env(fakeToken))
	if err != nil || cfg.LLMWebSearch != "off" || cfg.LLMWeightSearch != 8 {
		t.Errorf("defaults: %q, weight %d, %v", cfg.LLMWebSearch, cfg.LLMWeightSearch, err)
	}
	for _, mode := range []string{"off", "on-request", "always"} {
		if cfg, err := Parse([]byte("llm_web_search: "+mode+"\n"), env(fakeToken)); err != nil || cfg.LLMWebSearch != mode {
			t.Errorf("%s: %q, %v", mode, cfg.LLMWebSearch, err)
		}
	}
	for _, bad := range []string{"true", "sometimes"} {
		if _, err := Parse([]byte("llm_web_search: "+bad+"\n"), env(fakeToken)); err == nil || !strings.Contains(err.Error(), "off, on-request or always") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}
