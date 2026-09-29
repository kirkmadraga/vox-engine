package config

import (
	"bytes"
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
	DefaultMaxQueueLength = 10
	DefaultCacheMaxBytes  = 512 << 20 // 512 MiB
	DefaultCacheMaxAge    = 12 * time.Hour
	DefaultTestTone       = "assets/tone.opus" // committed with the code
)

// Config is the validated bot configuration.
type Config struct {
	Token string // from DISCORD_TOKEN

	OwnerIDs           []snowflake.ID
	CacheDir           string
	Database           string        // SQLite file: access lists and cache index
	CacheMaxBytes      int64         // 0 = no size cap
	CacheMaxAge        time.Duration // since last played; 0 = never expire
	TestTone           string        // Ogg Opus file played by "test"
	MaxConcurrentJobs  int
	MaxQueueLength     int // per server, including the playing track
	MaxDurationSeconds int
	YtdlpPath          string
	FfmpegPath         string
	YtdlpJSRuntime     string
	YtdlpCookies       string // optional cookies.txt for yt-dlp; must be writable (yt-dlp saves it back)
}

// file mirrors config.yaml. IDs stay strings here and are parsed to snowflakes after decoding.
type file struct {
	OwnerUserIDs       []string `yaml:"owner_user_ids"`
	CacheDir           string   `yaml:"cache_dir"`
	Database           string   `yaml:"database"`
	CacheMaxBytes      *int64   `yaml:"cache_max_bytes"`
	CacheMaxAge        *string  `yaml:"cache_max_age"`
	TestTone           string   `yaml:"test_tone"`
	MaxConcurrentJobs  *int     `yaml:"max_concurrent_jobs"`
	MaxQueueLength     *int     `yaml:"max_queue_length"`
	MaxDurationSeconds int      `yaml:"max_duration_seconds"`
	YtdlpPath          string   `yaml:"ytdlp_path"`
	FfmpegPath         string   `yaml:"ffmpeg_path"`
	YtdlpJSRuntime     string   `yaml:"ytdlp_js_runtime"`
	YtdlpCookies       string   `yaml:"ytdlp_cookies"`
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

	owners := make([]snowflake.ID, 0, len(f.OwnerUserIDs))
	for i, s := range f.OwnerUserIDs {
		id, err := snowflake.Parse(strings.TrimSpace(s))
		if err != nil || id == 0 {
			return Config{}, fmt.Errorf("owner_user_ids[%d]: %q is not a valid Discord ID", i, s)
		}
		owners = append(owners, id)
	}

	cfg := Config{
		Token:              token,
		OwnerIDs:           owners,
		CacheDir:           valueOr(f.CacheDir, "cache"),
		Database:           valueOr(f.Database, "data/bot.db"),
		CacheMaxBytes:      DefaultCacheMaxBytes,
		CacheMaxAge:        DefaultCacheMaxAge,
		TestTone:           valueOr(f.TestTone, DefaultTestTone),
		MaxConcurrentJobs:  1,
		MaxQueueLength:     DefaultMaxQueueLength,
		MaxDurationSeconds: f.MaxDurationSeconds,
		YtdlpPath:          f.YtdlpPath,
		FfmpegPath:         f.FfmpegPath,
		YtdlpJSRuntime:     f.YtdlpJSRuntime,
		YtdlpCookies:       strings.TrimSpace(f.YtdlpCookies),
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
