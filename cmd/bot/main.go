// Command bot is the entry point: load config, build the Discord client,
// register handlers, and run until interrupted.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	disgovoice "github.com/disgoorg/disgo/voice"

	"bot/internal/access"
	"bot/internal/access/jsonfile"
	"bot/internal/commands"
	"bot/internal/config"
	"bot/internal/discordio"
	"bot/internal/router"
	"bot/internal/voice"
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
	logger.Info("config loaded", "owners", len(cfg.OwnerIDs), "disgo", disgo.Version)
	logger.Debug("debug logging enabled")

	backend, err := jsonfile.Open(cfg.StateFile)
	if err != nil {
		return err
	}
	policy := access.NewPolicy(backend, access.Options{
		Owners:    cfg.OwnerIDs,
		Public:    []string{"ping"},
		OwnerOnly: commands.ManagementCommands,
		Logger:    logger,
	})
	if snap, err := policy.Snapshot(ctx); err == nil {
		logger.Info("access state loaded", "file", cfg.StateFile, "guilds", len(snap.Guilds), "grants", len(snap.Grants))
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

	// Playback is bound to ctx: on shutdown it stops and leaves the channel.
	player := voice.NewPlayer(ctx, discordio.VoiceConnector{Manager: client.VoiceManager, DAVE: dave}, nil, logger)
	tonePath := filepath.Join(cfg.CacheDir, "tone.opus")

	// Allow validates command names against the registry it is part of.
	var registry *commands.Registry
	known := func(name string) bool { _, ok := registry.Lookup(name); return ok }
	registry, err = commands.NewRegistry(
		commands.Ping{},
		commands.Play{Voice: discordio.VoiceStates{Caches: client.Caches}, Player: player, TonePath: tonePath, Logger: logger},
		commands.Allow{Access: policy, Known: known},
		commands.Deny{Access: policy},
		commands.AccessList{Access: policy},
	)
	if err != nil {
		return err
	}

	r := router.New(client.ID, registry, policy, logger)
	client.AddEventListeners(bot.NewListenerFunc(discordio.GuildMessageHandler(ctx, r, client.Rest)))

	if err := client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("connect to discord gateway: %w", err)
	}
	logger.Info("running; press Ctrl+C to stop")

	<-ctx.Done()
	logger.Info("shutting down")
	player.Wait() // ctx is cancelled, so playback stops and leaves voice first
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client.Close(closeCtx)
	return nil
}
