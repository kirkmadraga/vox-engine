package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // timezone names work even where the OS has no tz database (Windows, slim images)

	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Defaults for optional settings.
const (
	DefaultMaxQueueLength   = 10
	DefaultCacheMaxBytes    = 512 << 20 // 512 MiB
	DefaultCacheMaxAge      = 12 * time.Hour
	DefaultTestTone         = "assets/tone.opus" // committed with the code
	DefaultYtdlpMinInterval = 30 * time.Second
	DefaultSearchResults    = 10
	MaxSearchResults        = 10 // keeps the list within one Discord message
	DefaultVoiceIdleTimeout = 0  // leave as soon as the queue ends
	// DefaultUnauthorizedMessage answers a slash command the user may not run
	// (only they see it). Mentions from such users still get silence.
	DefaultUnauthorizedMessage = "You can't use that command here."
	maxUnauthorizedMessageLen  = 2000 // Discord's message limit
)

// Defaults for the ask command's load limits.
const (
	DefaultLLMUserCooldown    = 10 * time.Second
	DefaultLLMMaxConcurrent   = 3
	DefaultLLMHistoryMessages = 10
	DefaultLLMHistoryMaxAge   = 10 * time.Minute
	DefaultLLMTimeout         = 60 * time.Second
	DefaultLLMMaxPromptChars  = 500
	DefaultLLMDailyLimit      = 50
	DefaultLLMWeightSearch    = 8 // a search answer measured 5-9x a plain one (grok-4.3, 2026-09-30)
	DefaultLLMWeightImage     = 4
)

// LLMProviders are the accepted llm_provider values: "echo" (a stand-in that
// repeats the question), "xai" (Grok) and "openai", both through the
// Responses API.
var LLMProviders = []string{"echo", "xai", "openai"}

// Defaults for the model bridge.
const (
	DefaultLLMMaxTokens    = 500
	DefaultLLMMaxToolTurns = 2
)

// defaultLLMBaseURLs are the providers' API addresses; llm_base_url overrides.
var defaultLLMBaseURLs = map[string]string{
	"xai":    "https://api.x.ai/v1",
	"openai": "https://api.openai.com/v1",
}

// parseLLMBridge reads the model bridge's settings, which only matter when
// llm_provider is set. The API key comes from the environment (LLM_API_KEY).
func parseLLMBridge(cfg *Config, f file, getenv func(string) string) error {
	cfg.LLMMaxTokens = DefaultLLMMaxTokens
	cfg.LLMMaxToolTurns = DefaultLLMMaxToolTurns
	cfg.LLMWebSearch = strings.ToLower(strings.TrimSpace(f.LLMWebSearch))
	switch cfg.LLMWebSearch {
	case "":
		cfg.LLMWebSearch = "off"
	case "off", "on-request", "always":
	default:
		return fmt.Errorf("llm_web_search must be off, on-request or always, got %q", f.LLMWebSearch)
	}
	cfg.LLMWebSearchImages = f.LLMWebSearchImages
	cfg.LLMModel = strings.TrimSpace(f.LLMModel)
	cfg.LLMReasoningEffort = strings.ToLower(strings.TrimSpace(f.LLMReasoningEffort))
	if f.LLMMaxTokens != nil {
		if *f.LLMMaxTokens < 1 {
			return fmt.Errorf("llm_max_tokens must be at least 1, got %d", *f.LLMMaxTokens)
		}
		cfg.LLMMaxTokens = *f.LLMMaxTokens
	}
	if f.LLMMaxToolTurns != nil {
		if *f.LLMMaxToolTurns < 0 {
			return fmt.Errorf("llm_max_tool_turns must be 0 (no cap) or more, got %d", *f.LLMMaxToolTurns)
		}
		cfg.LLMMaxToolTurns = *f.LLMMaxToolTurns
	}
	switch cfg.LLMReasoningEffort {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("llm_reasoning_effort must be empty, low, medium or high, got %q", f.LLMReasoningEffort)
	}
	p := cfg.LLMProvider
	if p == "" {
		return nil
	}
	// The system prompt must be written down (even as ""), so a deployment
	// never runs a prompt its operator didn't choose.
	if f.LLMSystemPrompt == nil {
		return errors.New(`llm_system_prompt is required when llm_provider is set (use llm_system_prompt: "" for none; config.example.yaml has one to start from)`)
	}
	cfg.LLMSystemPrompt = strings.TrimSpace(*f.LLMSystemPrompt)
	if p == "echo" {
		return nil
	}
	cfg.LLMBaseURL = strings.TrimRight(strings.TrimSpace(f.LLMBaseURL), "/")
	if cfg.LLMBaseURL == "" {
		cfg.LLMBaseURL = defaultLLMBaseURLs[p]
	}
	if !strings.HasPrefix(cfg.LLMBaseURL, "https://") && !strings.HasPrefix(cfg.LLMBaseURL, "http://") {
		return fmt.Errorf("llm_base_url must start with https:// (or http:// for a local server), got %q", f.LLMBaseURL)
	}
	if cfg.LLMModel == "" {
		return fmt.Errorf("llm_model is required for llm_provider %q, e.g. grok-4.3", p)
	}
	cfg.LLMAPIKey = strings.TrimSpace(getenv("LLM_API_KEY"))
	if cfg.LLMAPIKey == "" {
		return fmt.Errorf("LLM_API_KEY is not set (put the %s API key in .env)", p)
	}
	return nil
}

// Config is the validated bot configuration.
type Config struct {
	Token string // from DISCORD_TOKEN

	OwnerIDs              []snowflake.ID
	CacheDir              string
	Database              string        // SQLite file: access lists and cache index
	CacheMaxBytes         int64         // 0 = no size cap
	CacheMaxAge           time.Duration // since last played; 0 = never expire
	TestTone              string        // Ogg Opus file played by "test"
	MaxConcurrentJobs     int
	MaxQueueLength        int           // per server, including the playing track
	SearchResults         int           // how many results "play <search words>" lists
	MaxVoiceSessions      int           // servers in voice at the same time
	VoiceIdleTimeout      time.Duration // how long to stay in voice after the queue ends; 0 = leave at once
	MaxConcurrentSearches int           // yt-dlp searches at the same time
	UnauthorizedMessage   string        // private reply to a slash command the user may not run
	MaxDurationSeconds    int
	YtdlpPath             string
	FFmpegPath            string
	YtdlpJSRuntime        string
	YtdlpCookies          string        // optional cookies.txt for yt-dlp; must be writable (yt-dlp saves it back)
	YtdlpMinInterval      time.Duration // minimum time between yt-dlp runs; 0 = none
	LLMProvider           string        // model behind the ask command; "" = off
	LLMUserCooldown       time.Duration // between one user's questions; 0 = none (owners exempt)
	LLMMaxConcurrent      int           // questions reaching the model at once
	LLMHistoryMessages    int           // messages remembered per channel; 0 = no memory
	LLMHistoryMaxAge      time.Duration // ...and only those newer than this; 0 = no age limit
	LLMTimeout            time.Duration // per question, waiting included
	LLMMaxPromptChars     int           // longest question, in characters; 0 = no limit
	LLMDailyLimit         int           // per user per day; 0 = no limit
	LLMDailyLimitOwners   bool          // owners are exempt unless true
	LLMDailyResetLocation *time.Location
	LLMWeightSearch       int // daily-limit cost of an answer that searched the web
	LLMWeightImage        int // ...that viewed images
	// ReminderLocation is where remindme's times are ("at 18:30"); the same
	// as LLMDailyResetLocation unless reminder_timezone is set.
	ReminderLocation *time.Location
	// DiscordMessageContent requests Discord's privileged Message Content
	// intent (it must be enabled in the Developer Portal first). Only ask uses
	// it, to read the message someone replies to when asking.
	DiscordMessageContent bool

	// The model bridge (ask); only used when LLMProvider is xai or openai.
	LLMBaseURL         string
	LLMModel           string
	LLMAPIKey          string // from LLM_API_KEY; never logged
	LLMSystemPrompt    string // "" = none
	LLMMaxTokens       int    // answer cap, reasoning included
	LLMReasoningEffort string // "" = the model's default
	LLMWebSearch       string // "off", "on-request" (the asker says "search") or "always" (the model decides)
	LLMWebSearchImages bool   // let search look at images it finds (xAI)
	LLMMaxToolTurns    int    // rounds of tool use per question; 0 = no cap
}

// file mirrors config.yaml. IDs stay strings here and are parsed to snowflakes after decoding.
type file struct {
	OwnerUserIDs          []string `yaml:"owner_user_ids"`
	CacheDir              string   `yaml:"cache_dir"`
	Database              string   `yaml:"database"`
	CacheMaxBytes         *int64   `yaml:"cache_max_bytes"`
	CacheMaxAge           *string  `yaml:"cache_max_age"`
	TestTone              string   `yaml:"test_tone"`
	MaxConcurrentJobs     *int     `yaml:"max_concurrent_jobs"`
	MaxQueueLength        *int     `yaml:"max_queue_length"`
	SearchResults         *int     `yaml:"search_results"`
	MaxVoiceSessions      *int     `yaml:"max_voice_sessions"`
	VoiceIdleTimeout      *string  `yaml:"voice_idle_timeout"`
	MaxConcurrentSearches *int     `yaml:"max_concurrent_searches"`
	UnauthorizedMessage   *string  `yaml:"unauthorized_message"`
	MaxDurationSeconds    int      `yaml:"max_duration_seconds"`
	YtdlpPath             string   `yaml:"ytdlp_path"`
	FFmpegPath            string   `yaml:"ffmpeg_path"`
	YtdlpJSRuntime        string   `yaml:"ytdlp_js_runtime"`
	YtdlpCookies          string   `yaml:"ytdlp_cookies"`
	YtdlpMinInterval      *string  `yaml:"ytdlp_min_interval"`
	LLMProvider           string   `yaml:"llm_provider"`
	LLMUserCooldown       *string  `yaml:"llm_user_cooldown"`
	LLMMaxConcurrent      *int     `yaml:"llm_max_concurrent"`
	LLMHistoryMessages    *int     `yaml:"llm_history_messages"`
	LLMHistoryMaxAge      *string  `yaml:"llm_history_max_age"`
	LLMTimeout            *string  `yaml:"llm_timeout"`
	LLMMaxPromptChars     *int     `yaml:"llm_max_prompt_chars"`
	LLMDailyLimit         *int     `yaml:"llm_daily_limit"`
	LLMDailyLimitOwners   bool     `yaml:"llm_daily_limit_owners"`
	LLMDailyResetTimezone string   `yaml:"llm_daily_reset_timezone"`
	LLMWeightSearch       *int     `yaml:"llm_weight_search"`
	LLMWeightImage        *int     `yaml:"llm_weight_image"`
	ReminderTimezone      string   `yaml:"reminder_timezone"`
	DiscordMessageContent bool     `yaml:"discord_message_content"`
	LLMBaseURL            string   `yaml:"llm_base_url"`
	LLMModel              string   `yaml:"llm_model"`
	LLMSystemPrompt       *string  `yaml:"llm_system_prompt"`
	LLMMaxTokens          *int     `yaml:"llm_max_tokens"`
	LLMReasoningEffort    string   `yaml:"llm_reasoning_effort"`
	LLMWebSearch          string   `yaml:"llm_web_search"`
	LLMWebSearchImages    bool     `yaml:"llm_web_search_images"`
	LLMMaxToolTurns       *int     `yaml:"llm_max_tool_turns"`
}

// LoadDotEnv loads KEY=VALUE pairs from path into the process environment.
// A missing file is not an error (systemd may inject the variables instead),
// and variables already set in the environment are not overridden.
func LoadDotEnv(path string) error {
	err := godotenv.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	return nil
}

// Load reads and validates the YAML config at yamlPath. Secrets come from getenv.
func Load(yamlPath string, getenv func(string) string) (Config, error) {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, getenv)
}

// Parse validates YAML config bytes. Secrets come from getenv.
func Parse(data []byte, getenv func(string) string) (Config, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // catch typos in hand-edited keys
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	token := strings.TrimSpace(getenv("DISCORD_TOKEN"))
	if token == "" {
		return Config{}, errors.New("DISCORD_TOKEN is not set (put it in .env or the environment)")
	}
	if !plausibleToken(token) {
		return Config{}, errors.New("DISCORD_TOKEN doesn't look like a bot token; copy it again from the Bot page of the Discord developer portal")
	}

	owners := make([]snowflake.ID, 0, len(f.OwnerUserIDs))
	for i, s := range f.OwnerUserIDs {
		s = strings.TrimSpace(s)
		if exampleOwnerIDs[s] {
			return Config{}, fmt.Errorf("owner_user_ids[%d] is still the example value %s; replace it with your Discord user ID", i, s)
		}
		id, err := snowflake.Parse(s)
		if err != nil || id == 0 {
			return Config{}, fmt.Errorf("owner_user_ids[%d]: %q is not a valid Discord ID", i, s)
		}
		owners = append(owners, id)
	}

	cfg := Config{
		Token:                 token,
		OwnerIDs:              owners,
		CacheDir:              valueOr(f.CacheDir, "cache"),
		Database:              valueOr(f.Database, "data/bot.db"),
		CacheMaxBytes:         DefaultCacheMaxBytes,
		CacheMaxAge:           DefaultCacheMaxAge,
		TestTone:              valueOr(f.TestTone, DefaultTestTone),
		MaxConcurrentJobs:     1,
		MaxQueueLength:        DefaultMaxQueueLength,
		SearchResults:         DefaultSearchResults,
		MaxVoiceSessions:      1,
		VoiceIdleTimeout:      DefaultVoiceIdleTimeout,
		MaxConcurrentSearches: 1,
		UnauthorizedMessage:   DefaultUnauthorizedMessage,
		MaxDurationSeconds:    f.MaxDurationSeconds,
		YtdlpPath:             f.YtdlpPath,
		FFmpegPath:            f.FFmpegPath,
		YtdlpJSRuntime:        f.YtdlpJSRuntime,
		YtdlpCookies:          strings.TrimSpace(f.YtdlpCookies),
		YtdlpMinInterval:      DefaultYtdlpMinInterval,
		LLMProvider:           strings.ToLower(strings.TrimSpace(f.LLMProvider)),
		LLMUserCooldown:       DefaultLLMUserCooldown,
		LLMMaxConcurrent:      DefaultLLMMaxConcurrent,
		LLMHistoryMessages:    DefaultLLMHistoryMessages,
		LLMHistoryMaxAge:      DefaultLLMHistoryMaxAge,
		LLMTimeout:            DefaultLLMTimeout,
		LLMMaxPromptChars:     DefaultLLMMaxPromptChars,
		LLMDailyLimit:         DefaultLLMDailyLimit,
		LLMDailyLimitOwners:   f.LLMDailyLimitOwners,
		LLMDailyResetLocation: time.UTC,
		LLMWeightSearch:       DefaultLLMWeightSearch,
		LLMWeightImage:        DefaultLLMWeightImage,
		DiscordMessageContent: f.DiscordMessageContent,
	}
	if f.LLMTimeout != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*f.LLMTimeout))
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("llm_timeout must be a duration like \"60s\", got %q", *f.LLMTimeout)
		}
		cfg.LLMTimeout = d
	}
	nonNegative := []struct {
		key string
		in  *int
		out *int
	}{
		{"llm_max_prompt_chars", f.LLMMaxPromptChars, &cfg.LLMMaxPromptChars},
		{"llm_daily_limit", f.LLMDailyLimit, &cfg.LLMDailyLimit},
		{"llm_weight_search", f.LLMWeightSearch, &cfg.LLMWeightSearch},
		{"llm_weight_image", f.LLMWeightImage, &cfg.LLMWeightImage},
	}
	for _, n := range nonNegative {
		if n.in == nil {
			continue
		}
		if *n.in < 0 {
			return Config{}, fmt.Errorf("%s must be 0 or more, got %d", n.key, *n.in)
		}
		*n.out = *n.in
	}
	if tz := strings.TrimSpace(f.LLMDailyResetTimezone); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return Config{}, fmt.Errorf("llm_daily_reset_timezone %q isn't a known timezone (use a name like \"Europe/Berlin\" or \"UTC\")", tz)
		}
		cfg.LLMDailyResetLocation = loc
	}
	cfg.ReminderLocation = cfg.LLMDailyResetLocation
	if tz := strings.TrimSpace(f.ReminderTimezone); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return Config{}, fmt.Errorf("reminder_timezone %q isn't a known timezone (use a name like \"Europe/Berlin\" or \"UTC\")", tz)
		}
		cfg.ReminderLocation = loc
	}
	if f.LLMHistoryMessages != nil {
		if *f.LLMHistoryMessages < 0 {
			return Config{}, fmt.Errorf("llm_history_messages must be 0 (no memory) or more, got %d", *f.LLMHistoryMessages)
		}
		cfg.LLMHistoryMessages = *f.LLMHistoryMessages
	}
	if f.LLMHistoryMaxAge != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*f.LLMHistoryMaxAge))
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("llm_history_max_age must be a duration like \"10m\" (0 = no age limit), got %q", *f.LLMHistoryMaxAge)
		}
		cfg.LLMHistoryMaxAge = d
	}
	if f.LLMUserCooldown != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*f.LLMUserCooldown))
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("llm_user_cooldown must be a duration like \"10s\" (0 = none), got %q", *f.LLMUserCooldown)
		}
		cfg.LLMUserCooldown = d
	}
	if f.LLMMaxConcurrent != nil {
		if *f.LLMMaxConcurrent < 1 {
			return Config{}, fmt.Errorf("llm_max_concurrent must be at least 1, got %d", *f.LLMMaxConcurrent)
		}
		cfg.LLMMaxConcurrent = *f.LLMMaxConcurrent
	}
	if p := cfg.LLMProvider; p != "" && !slices.Contains(LLMProviders, p) {
		return Config{}, fmt.Errorf("llm_provider must be empty (off) or one of %s, got %q", strings.Join(LLMProviders, ", "), f.LLMProvider)
	}
	if err := parseLLMBridge(&cfg, f, getenv); err != nil {
		return Config{}, err
	}
	if f.MaxConcurrentJobs != nil {
		if *f.MaxConcurrentJobs < 1 {
			return Config{}, fmt.Errorf("max_concurrent_jobs must be at least 1, got %d", *f.MaxConcurrentJobs)
		}
		cfg.MaxConcurrentJobs = *f.MaxConcurrentJobs
	}
	if f.MaxQueueLength != nil {
		if *f.MaxQueueLength < 1 {
			return Config{}, fmt.Errorf("max_queue_length must be at least 1, got %d", *f.MaxQueueLength)
		}
		cfg.MaxQueueLength = *f.MaxQueueLength
	}
	if f.SearchResults != nil {
		if n := *f.SearchResults; n < 1 || n > MaxSearchResults {
			return Config{}, fmt.Errorf("search_results must be 1 to %d, got %d", MaxSearchResults, n)
		}
		cfg.SearchResults = *f.SearchResults
	}
	if f.MaxVoiceSessions != nil {
		if *f.MaxVoiceSessions < 1 {
			return Config{}, fmt.Errorf("max_voice_sessions must be at least 1, got %d", *f.MaxVoiceSessions)
		}
		cfg.MaxVoiceSessions = *f.MaxVoiceSessions
	}
	if f.MaxConcurrentSearches != nil {
		if *f.MaxConcurrentSearches < 1 {
			return Config{}, fmt.Errorf("max_concurrent_searches must be at least 1, got %d", *f.MaxConcurrentSearches)
		}
		cfg.MaxConcurrentSearches = *f.MaxConcurrentSearches
	}
	if f.UnauthorizedMessage != nil {
		msg := strings.TrimSpace(*f.UnauthorizedMessage)
		switch {
		case msg == "":
			return Config{}, errors.New("unauthorized_message can't be empty (Discord needs some reply to a slash command); remove it to use the default")
		case len([]rune(msg)) > maxUnauthorizedMessageLen:
			return Config{}, fmt.Errorf("unauthorized_message is longer than %d characters", maxUnauthorizedMessageLen)
		}
		cfg.UnauthorizedMessage = msg
	}
	if f.VoiceIdleTimeout != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*f.VoiceIdleTimeout))
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("voice_idle_timeout must be a duration like \"2m\" or \"30s\" (0 = leave at once), got %q", *f.VoiceIdleTimeout)
		}
		cfg.VoiceIdleTimeout = d
	}
	if f.CacheMaxBytes != nil {
		if *f.CacheMaxBytes < 0 {
			return Config{}, fmt.Errorf("cache_max_bytes must be 0 (no cap) or positive, got %d", *f.CacheMaxBytes)
		}
		cfg.CacheMaxBytes = *f.CacheMaxBytes
	}
	if f.CacheMaxAge != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*f.CacheMaxAge))
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("cache_max_age must be a duration like \"12h\" or \"30m\" (0 = never), got %q", *f.CacheMaxAge)
		}
		cfg.CacheMaxAge = d
	}
	if f.YtdlpMinInterval != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*f.YtdlpMinInterval))
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("ytdlp_min_interval must be a duration like \"30s\" (0 = none), got %q", *f.YtdlpMinInterval)
		}
		cfg.YtdlpMinInterval = d
	}
	if cfg.MaxDurationSeconds < 0 {
		return Config{}, fmt.Errorf("max_duration_seconds must be 0 (no limit) or positive, got %d", cfg.MaxDurationSeconds)
	}
	return cfg, nil
}

func valueOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// exampleOwnerIDs are the placeholders in config.example.yaml. Left in place,
// they'd start a bot with owners nobody is, which then silently ignores its
// real owner.
var exampleOwnerIDs = map[string]bool{"111111111111111111": true, "222222222222222222": true}

// plausibleToken reports whether s has the shape of a Discord bot token:
// three dot-separated parts, the first being the bot's numeric ID in base64.
// It catches paste mistakes early; Discord itself is the final judge.
func plausibleToken(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return false
	}
	id, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	if err != nil {
		id, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	}
	if err != nil || len(id) == 0 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
