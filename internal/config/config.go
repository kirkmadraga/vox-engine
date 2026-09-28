package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Config is the validated bot configuration.
type Config struct {
	Token string // from DISCORD_TOKEN

	OwnerIDs           []snowflake.ID
	CacheDir           string
	StateFile          string
	MaxConcurrentJobs  int
	MaxDurationSeconds int
	YtdlpPath          string
	FfmpegPath         string
	YtdlpJSRuntime     string
}

// file mirrors config.yaml. IDs stay strings here and are parsed to snowflakes after decoding.
type file struct {
	OwnerUserIDs       []string `yaml:"owner_user_ids"`
	CacheDir           string   `yaml:"cache_dir"`
	StateFile          string   `yaml:"state_file"`
	MaxConcurrentJobs  *int     `yaml:"max_concurrent_jobs"`
	MaxDurationSeconds int      `yaml:"max_duration_seconds"`
	YtdlpPath          string   `yaml:"ytdlp_path"`
	FfmpegPath         string   `yaml:"ffmpeg_path"`
	YtdlpJSRuntime     string   `yaml:"ytdlp_js_runtime"`
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
		StateFile:          valueOr(f.StateFile, "data/access.json"),
		MaxConcurrentJobs:  1,
		MaxDurationSeconds: f.MaxDurationSeconds,
		YtdlpPath:          f.YtdlpPath,
		FfmpegPath:         f.FfmpegPath,
		YtdlpJSRuntime:     f.YtdlpJSRuntime,
	}
	if f.MaxConcurrentJobs != nil {
		if *f.MaxConcurrentJobs < 1 {
			return Config{}, fmt.Errorf("max_concurrent_jobs must be at least 1, got %d", *f.MaxConcurrentJobs)
		}
		cfg.MaxConcurrentJobs = *f.MaxConcurrentJobs
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
