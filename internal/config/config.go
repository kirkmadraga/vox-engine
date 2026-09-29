package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

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
	FfmpegPath            string
	YtdlpJSRuntime        string
	YtdlpCookies          string        // optional cookies.txt for yt-dlp; must be writable (yt-dlp saves it back)
	YtdlpMinInterval      time.Duration // minimum time between yt-dlp runs; 0 = none
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
	FfmpegPath            string   `yaml:"ffmpeg_path"`
	YtdlpJSRuntime        string   `yaml:"ytdlp_js_runtime"`
	YtdlpCookies          string   `yaml:"ytdlp_cookies"`
	YtdlpMinInterval      *string  `yaml:"ytdlp_min_interval"`
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
			return Config{}, fmt.Errorf("owner_user_ids[%d] is still the example value %s; replace it with your Discord user ID (Developer Mode, then right-click yourself, Copy User ID)", i, s)
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
		FfmpegPath:            f.FfmpegPath,
		YtdlpJSRuntime:        f.YtdlpJSRuntime,
		YtdlpCookies:          strings.TrimSpace(f.YtdlpCookies),
		YtdlpMinInterval:      DefaultYtdlpMinInterval,
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
			return Config{}, fmt.Errorf("unauthorized_message can't be empty (Discord needs some reply to a slash command); remove it to use the default")
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
