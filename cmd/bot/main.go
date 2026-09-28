// Command bot is the entry point: load config, build the Discord client,
// register handlers, and run until interrupted.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	disgovoice "github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"bot/internal/access"
	"bot/internal/access/jsonfile"
	audiocache "bot/internal/cache"
	"bot/internal/commands"
	"bot/internal/config"
	"bot/internal/discordio"
	"bot/internal/queue"
	"bot/internal/router"
	"bot/internal/voice"
	"bot/internal/ytdlp"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	envPath := flag.String("env", ".env", "path to .env (optional; environment variables take precedence)")
	debug := flag.Bool("debug", false, "log why messages are ignored (never logs message content)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	// disgo stays at info even with -debug; its debug output is gateway noise.
	libLogger := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("lib", "disgo")

	// os.Interrupt covers Ctrl+C on Windows; SIGTERM covers systemd on Linux.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger, libLogger, *configPath, *envPath); err != nil {
		logger.Error("fatal", "err", err)
		stop()
		os.Exit(1)
	}
}

// run holds all startup and shutdown logic so it can return errors instead of exiting.
func run(ctx context.Context, logger, libLogger *slog.Logger, configPath, envPath string) error {
	if err := config.LoadDotEnv(envPath); err != nil {
		return err
	}
	cfg, err := config.Load(configPath, os.Getenv)
	if err != nil {
		return err
	}
	logger.Info("config loaded", "owners", len(cfg.OwnerIDs), "max_queue_length", cfg.MaxQueueLength, "disgo", disgo.Version)
	logger.Debug("debug logging enabled")

	backend, err := jsonfile.Open(cfg.StateFile)
	if err != nil {
		return err
	}
	inherit := map[string]string{}
	for _, name := range commands.PlaybackCommands {
		inherit[name] = "play" // test, queue, skip, stop: whoever may play may use them
	}
	policy := access.NewPolicy(backend, access.Options{
		Owners:    cfg.OwnerIDs,
		Public:    []string{"ping"},
		OwnerOnly: commands.ManagementCommands,
		Inherit:   inherit,
		Logger:    logger,
	})
	if snap, err := policy.Snapshot(ctx); err == nil {
		logger.Info("access state loaded", "file", cfg.StateFile, "guilds", len(snap.Guilds), "grants", len(snap.Grants))
	}

	// Downloads: yt-dlp (plus ffmpeg, which yt-dlp calls) into the audio cache.
	downloader := ytdlp.Downloader{
		Runner:         ytdlp.ExecRunner{},
		Path:           findTool(logger, "yt-dlp", cfg.YtdlpPath),
		FFmpegLocation: findTool(logger, "ffmpeg", cfg.FfmpegPath),
		JSRuntime:      cfg.YtdlpJSRuntime,
		MaxDuration:    time.Duration(cfg.MaxDurationSeconds) * time.Second,
	}
	if downloader.JSRuntime == "" {
		logger.Warn("ytdlp_js_runtime is not set; YouTube downloads may fail or miss formats")
	}
	audio, err := audiocache.New(ctx, cfg.CacheDir, audiocache.Options{
		Fetcher:      downloader,
		Validate:     voice.ValidateFile,
		MaxDownloads: cfg.MaxConcurrentJobs,
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	if size, err := audio.Size(); err == nil {
		logger.Info("audio cache", "dir", cfg.CacheDir, "bytes", size)
	}
	youtube := func(id string) queue.LoadFunc {
		return func(ctx context.Context) (queue.Loaded, error) {
			e, _, err := audio.Get(ctx, id)
			return queue.Loaded{Path: e.Path, Title: e.Title, Duration: e.Duration}, err
		}
	}
	tonePath := filepath.Join(cfg.CacheDir, "tone.opus")
	tone := func(context.Context) (queue.Loaded, error) {
		if _, err := os.Stat(tonePath); err != nil {
			return queue.Loaded{}, fmt.Errorf("test tone (generate it with scripts/make-tone.ps1): %w", err)
		}
		return queue.Loaded{Path: tonePath, Title: "Test tone"}, nil
	}

	// Voice: dave-go provides DAVE end-to-end encryption, which Discord requires.
	dave := &discordio.DAVE{}
	client, err := disgo.New(cfg.Token,
		bot.WithLogger(libLogger),
		bot.WithGatewayConfigOpts(gateway.WithIntents(
			gateway.IntentGuilds,
			gateway.IntentGuildMessages,
			gateway.IntentGuildVoiceStates, // voice connections, and finding the caller's channel
		)),
		// Only voice states are cached: play needs the caller's current channel.
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagVoiceStates)),
		bot.WithVoiceManagerConfigOpts(disgovoice.WithDaveSessionCreateFunc(dave.CreateFunc())),
		bot.WithEventManagerConfigOpts(bot.WithAsyncEventsEnabled()),
	)
	if err != nil {
		return fmt.Errorf("create discord client: %w", err)
	}
	logger.Info("voice: DAVE enabled via dave-go")

	// Playback is bound to ctx: on shutdown it stops and leaves voice.
	q := queue.New(ctx, queue.Options{
		Connector: discordio.VoiceConnector{Manager: client.VoiceManager, DAVE: dave},
		Notifier:  discordio.ChannelNotifier{Sender: client.Rest, Logger: logger},
		Logger:    logger,
		MaxLength: cfg.MaxQueueLength,
	})
	voiceStates := discordio.VoiceStates{Caches: client.Caches}

	// Allow validates command names against the registry it is part of.
	var registry *commands.Registry
	known := func(name string) bool { _, ok := registry.Lookup(name); return ok }
	registry, err = commands.NewRegistry(
		commands.Ping{},
		commands.Play{Voice: voiceStates, Queue: q, YouTube: youtube},
		commands.Test{Voice: voiceStates, Queue: q, Tone: tone},
		commands.QueueList{Queue: q},
		commands.Skip{Queue: q},
		commands.Stop{Queue: q},
		commands.Allow{Access: policy, Known: known},
		commands.Deny{Access: policy},
		commands.AccessList{Access: policy},
	)
	if err != nil {
		return err
	}

	r := router.New(client.ID, registry, policy, logger)
	client.AddEventListeners(
		bot.NewListenerFunc(discordio.GuildMessageHandler(ctx, r, client.Rest)),
		// Removed from voice by someone else: discard that server's queue.
		bot.NewListenerFunc(discordio.BotVoiceLeaveHandler(client.ID, func(guildID snowflake.ID) { q.Disconnected(guildID) })),
	)

	if err := client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("connect to discord gateway: %w", err)
	}
	logger.Info("running; press Ctrl+C to stop")

	<-ctx.Done()
	logger.Info("shutting down")
	q.Wait() // ctx is cancelled, so playback stops, voice is left, and downloads end
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client.Close(closeCtx)
	return nil
}

// findTool returns the configured path for a tool, or finds it on PATH
// (exec.LookPath handles .exe on Windows). A missing tool is logged, not
// fatal: ping and access management still work, and downloads report it.
func findTool(logger *slog.Logger, name, configured string) string {
	if configured != "" {
		return configured
	}
	path, err := exec.LookPath(name)
	if err != nil {
		logger.Error("tool not found on PATH; set its path in config.yaml", "tool", name)
		return name
	}
	logger.Info("tool found", "tool", name, "path", path)
	return path
}
