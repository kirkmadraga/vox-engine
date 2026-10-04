// Command vox-engine is the entry point: load config, build the Discord client,
// register handlers, and run until interrupted.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/gateway"
	disgovoice "github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"

	"github.com/kirkmadraga/vox-engine/internal/access"

	audiocache "github.com/kirkmadraga/vox-engine/internal/cache"
	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/config"
	"github.com/kirkmadraga/vox-engine/internal/discordio"
	"github.com/kirkmadraga/vox-engine/internal/llm"
	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/router"
	"github.com/kirkmadraga/vox-engine/internal/spotify"
	"github.com/kirkmadraga/vox-engine/internal/store"
	"github.com/kirkmadraga/vox-engine/internal/version"
	"github.com/kirkmadraga/vox-engine/internal/voice"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	envPath := flag.String("env", ".env", "path to .env (optional; environment variables take precedence)")
	debug := flag.Bool("debug", false, "log why messages are ignored (never logs message content)")
	checkConfig := flag.Bool("check-config", false, "load .env and config.yaml, report whether they're valid, and exit")
	flag.Parse()

	if *checkConfig {
		if _, err := loadConfig(*configPath, *envPath); err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(1)
		}
		fmt.Println("config OK")
		return
	}

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

// loadConfig loads .env (into the environment) and config.yaml, as startup does.
func loadConfig(configPath, envPath string) (config.Config, error) {
	if err := config.LoadDotEnv(envPath); err != nil {
		return config.Config{}, err
	}
	return config.Load(configPath, os.Getenv)
}

// intents are the gateway intents the bot asks for.
func intents(cfg config.Config) []gateway.Intents {
	in := []gateway.Intents{
		gateway.IntentGuilds,
		gateway.IntentGuildMessages,
		gateway.IntentGuildVoiceStates, // voice connections, and finding the caller's channel
	}
	if cfg.DiscordMessageContent {
		in = append(in, gateway.IntentMessageContent) // privileged: read replied-to messages for ask
	}
	return in
}

// run holds all startup and shutdown logic so it can return errors instead of exiting.
func run(ctx context.Context, logger, libLogger *slog.Logger, configPath, envPath string) error {
	build := version.Read()
	logger.Info("starting", "version", build.Name(), "commit", build.Commit, "modified", build.Modified, "go", build.Go)
	cfg, err := loadConfig(configPath, envPath)
	if err != nil {
		return err
	}
	logger.Info("config loaded", "owners", len(cfg.OwnerIDs), "max_queue_length", cfg.MaxQueueLength,
		"max_voice_sessions", cfg.MaxVoiceSessions, "voice_idle_timeout", cfg.VoiceIdleTimeout,
		"max_concurrent_searches", cfg.MaxConcurrentSearches, "llm_provider", cfg.LLMProvider,
		"llm_user_cooldown", cfg.LLMUserCooldown, "llm_max_concurrent", cfg.LLMMaxConcurrent,
		"llm_history_messages", cfg.LLMHistoryMessages, "llm_history_max_age", cfg.LLMHistoryMaxAge,
		"llm_timeout", cfg.LLMTimeout, "llm_max_prompt_chars", cfg.LLMMaxPromptChars,
		"llm_daily_limit", cfg.LLMDailyLimit, "llm_daily_limit_owners", cfg.LLMDailyLimitOwners,
		"llm_daily_reset_timezone", cfg.LLMDailyResetLocation, "llm_weights", fmt.Sprintf("search=%d image=%d", cfg.LLMWeightSearch, cfg.LLMWeightImage),
		"discord_message_content", cfg.DiscordMessageContent,
		"llm_model", cfg.LLMModel, "llm_max_tokens", cfg.LLMMaxTokens, "llm_reasoning_effort", cfg.LLMReasoningEffort,
		"llm_web_search", cfg.LLMWebSearch, "llm_web_search_images", cfg.LLMWebSearchImages, "llm_max_tool_turns", cfg.LLMMaxToolTurns, "disgo", disgo.Version)
	logger.Debug("debug logging enabled")

	// One SQLite database holds the access lists and the audio cache index.
	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Database), "access.json")); err == nil {
		logger.Warn("data/access.json is no longer used (access lists now live in the database); re-add servers and grants with allow, then delete the file")
	}

	inherit := map[string]string{}
	for _, name := range commands.PlaybackCommands {
		inherit[name] = "play" // test, queue, skip, stop: whoever may play may use them
	}
	inherit[commands.ForgetCommand] = commands.AskCommand // forget works where ask works
	policy := access.NewPolicy(db.Access(), access.Options{
		Owners:    cfg.OwnerIDs,
		Public:    []string{"ping"},
		OwnerOnly: commands.ManagementCommands,
		Inherit:   inherit,
		// ask runs only in channels an owner allowed (see access.Policy).
		ChannelLimited: []string{commands.AskCommand},
		Logger:         logger,
	})
	if snap, err := policy.Snapshot(ctx); err == nil {
		logger.Info("database opened", "file", cfg.Database, "guilds", len(snap.Guilds), "grants", len(snap.Grants))
	}

	// Downloads: yt-dlp (plus ffmpeg, which yt-dlp calls) into the audio cache.
	ytRecent := &ytdlp.Recent{} // the last download and search, for "debug ytdlp"
	downloader := ytdlp.Downloader{
		Recent:         ytRecent,
		Runner:         ytdlp.ExecRunner{},
		Path:           findTool(logger, "yt-dlp", cfg.YtdlpPath),
		FFmpegLocation: findTool(logger, "ffmpeg", cfg.FFmpegPath),
		JSRuntime:      cfg.YtdlpJSRuntime,
		Cookies:        cfg.YtdlpCookies,
		MaxDuration:    time.Duration(cfg.MaxDurationSeconds) * time.Second,
		Logger:         logger,
	}
	// yt-dlp's version, for "debug" and the log. Asked once now in the
	// background (it takes seconds on a slow CPU), then kept for a while.
	ytVersion := &ytdlp.VersionCache{Ask: downloader.Version}
	go func() {
		if v, err := ytVersion.Version(ctx); err != nil {
			if ctx.Err() == nil { // not just a quick shutdown
				logger.Warn("ytdlp: couldn't ask for its version", "err", err)
			}
		} else {
			logger.Info("ytdlp: version", "version", v)
		}
	}()
	searcher := &ytdlp.Searcher{
		Recent:        ytRecent,
		MaxConcurrent: cfg.MaxConcurrentSearches,
		Logger:        logger,
		Runner:        downloader.Runner,
		Path:          downloader.Path,
		JSRuntime:     downloader.JSRuntime,
		Cookies:       downloader.Cookies,
	}
	if downloader.JSRuntime == "" {
		logger.Warn("ytdlp_js_runtime is not set; YouTube downloads may fail or miss formats")
	}
	if cfg.YtdlpCookies != "" {
		checkCookies(logger, cfg.YtdlpCookies)
		// Each yt-dlp run uses a private copy of the cookies file; this lock
		// guards the real file while it's copied or replaced.
		cookiesLock := &sync.Mutex{}
		downloader.Lock, searcher.Lock = cookiesLock, cookiesLock
		if err := ytdlp.RemoveStaleCookieCopies(cfg.YtdlpCookies); err != nil {
			logger.Warn("couldn't remove leftover cookie copies", "err", err)
		}
	}
	// The queue is created later (it needs the Discord client); until then nothing is queued.
	var q *queue.Manager
	audio, err := audiocache.New(ctx, cfg.CacheDir, audiocache.Options{
		Fetcher:      downloader,
		Index:        db.CacheIndex(),
		Validate:     voice.ValidateFile,
		MaxDownloads: cfg.MaxConcurrentJobs,
		MaxBytes:     cfg.CacheMaxBytes,
		MaxAge:       cfg.CacheMaxAge,
		MinInterval:  cfg.YtdlpMinInterval,
		InUse: func() map[string]bool {
			if q == nil {
				return nil
			}
			return q.InUse()
		},
		Logger: logger,
	})
	if err != nil {
		return err
	}
	if size, err := audio.Size(ctx); err == nil {
		logger.Info("audio cache", "dir", cfg.CacheDir, "bytes", size, "max_bytes", cfg.CacheMaxBytes,
			"max_age", cfg.CacheMaxAge, "download_interval", cfg.YtdlpMinInterval)
	}
	youtube := func(id string) queue.LoadFunc {
		return func(ctx context.Context) (queue.Loaded, error) {
			e, _, err := audio.Get(ctx, id)
			return queue.Loaded{Path: e.Path, Title: e.Title, Duration: e.Duration, OnStart: func() { audio.MarkPlayed(id) }}, err
		}
	}
	tonePath := cfg.TestTone
	tone := func(context.Context) (queue.Loaded, error) {
		if _, err := os.Stat(tonePath); err != nil {
			return queue.Loaded{}, fmt.Errorf("test tone (set test_tone in config.yaml): %w", err)
		}
		return queue.Loaded{Path: tonePath, Title: "Test tone"}, nil
	}

	// Voice: dave-go provides DAVE end-to-end encryption, which Discord requires.
	dave := &discordio.DAVE{}
	client, err := disgo.New(cfg.Token,
		bot.WithLogger(libLogger),
		bot.WithGatewayConfigOpts(gateway.WithIntents(intents(cfg)...)),
		// Voice states: play needs the caller's current channel. Channels
		// (threads included): a thread counts as its parent for ask channels,
		// and "allow ask" says when the bot can't see a channel.
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagVoiceStates, cache.FlagChannels)),
		bot.WithVoiceManagerConfigOpts(disgovoice.WithDaveSessionCreateFunc(dave.CreateFunc())),
		bot.WithEventManagerConfigOpts(bot.WithAsyncEventsEnabled()),
	)
	if err != nil {
		return fmt.Errorf("create discord client: %w", err)
	}
	logger.Info("voice: DAVE enabled via dave-go")

	// Playback is bound to ctx: on shutdown it stops and leaves voice.
	q = queue.New(ctx, queue.Options{
		Connector:   discordio.VoiceConnector{Manager: client.VoiceManager, DAVE: dave},
		Notifier:    discordio.ChannelNotifier{Sender: client.Rest, Logger: logger},
		Logger:      logger,
		MaxLength:   cfg.MaxQueueLength,
		MaxSessions: cfg.MaxVoiceSessions,
		IdleTimeout: cfg.VoiceIdleTimeout,
	})
	voiceStates := discordio.VoiceStates{Caches: client.Caches}

	recent := commands.NewRecentSearches(0, nil) // per-user results for "play <number>"

	// Allow validates command names against the registry it is part of.
	var registry *commands.Registry
	known := func(name string) bool { _, ok := registry.Lookup(name); return ok }
	cmds := []commands.Command{
		commands.Ping{},
		commands.Play{
			Voice: voiceStates, Queue: q, YouTube: youtube,
			Search: searcher.SearchN, Results: cfg.SearchResults, Recent: recent,
			Spotify: (&spotify.Client{}).Track, SearchMusic: searcher.SearchMusic, Logger: logger,
		},
		commands.Test{Voice: voiceStates, Queue: q, Tone: tone},
		commands.QueueList{Queue: q},
		commands.Skip{Queue: q},
		commands.Stop{Queue: q},
		commands.Allow{Access: policy, Known: known, ChannelVisible: func(id snowflake.ID) bool {
			_, ok := client.Caches.Channel(id)
			return ok
		}},
		commands.Deny{Access: policy},
	}
	// Owner-only status ("debug …"), mentions only: not a slash command.
	debug := commands.Debug{
		Access: policy, Queue: q.Status, Cache: audio.Status, Search: searcher.Status, Recent: ytRecent,
		Started: time.Now(), Version: build.String(), YtdlpVersion: ytVersion.Version,
	}
	// The LLM bridge: "ask", also reached by mentions that start with no command
	// word. Off (not registered) unless llm_provider is set.
	grantable := []string{commands.DefaultGrantCommand} // the rest share play's grant, are public, or owner-only
	askOn := cfg.LLMProvider != ""
	// The channel's recent ask conversation (RAM only; see ChannelMemory).
	memory := &commands.ChannelMemory{MaxMessages: cfg.LLMHistoryMessages, MaxAge: cfg.LLMHistoryMaxAge}
	answers := &commands.AnswerLog{} // a reply to one of these continues the conversation
	if askOn {
		provider, err := llm.New(llm.Settings{
			Provider: cfg.LLMProvider, BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, APIKey: cfg.LLMAPIKey,
			Instructions: cfg.LLMSystemPrompt, MaxOutputTokens: cfg.LLMMaxTokens, ReasoningEffort: cfg.LLMReasoningEffort,
			WebSearch: cfg.LLMWebSearch != "off", ImageUnderstanding: cfg.LLMWebSearchImages, MaxTurns: cfg.LLMMaxToolTurns,
			Logger: logger,
		})
		if err != nil {
			return err
		}
		askCmd := &commands.Ask{
			LLM: provider, Logger: logger,
			Cooldown: cfg.LLMUserCooldown, MaxConcurrent: cfg.LLMMaxConcurrent, IsOwner: policy.IsOwner,
			MaxPromptChars: cfg.LLMMaxPromptChars, Timeout: cfg.LLMTimeout, Search: cfg.LLMWebSearch,
			Daily: &commands.DailyLimit{
				Limit: cfg.LLMDailyLimit, LimitOwners: cfg.LLMDailyLimitOwners, IsOwner: policy.IsOwner,
				Location: cfg.LLMDailyResetLocation, Store: db,
				WeightSearch: cfg.LLMWeightSearch, WeightImage: cfg.LLMWeightImage,
			},
			Memory: memory, Answers: answers,
		}
		cmds = append(cmds, askCmd, commands.Forget{Memory: memory})
		debug.LLM, debug.Ask = provider, askCmd
		// ask's own limit, plus time to send the reply (or the "couldn't answer" error).
		discordio.HandleTimeout = max(discordio.HandleTimeout, cfg.LLMTimeout+15*time.Second)
		grantable = append(grantable, commands.AskCommand) // its own grant: it may cost money
	}
	cmds = append(cmds, debug)
	registry, err = commands.NewRegistry(cmds...)
	if err != nil {
		return err
	}

	r := router.New(client.ID, registry, policy, logger)
	r.Fallback = commands.AskCommand
	r.IsAnswer = answers.Has
	r.ThreadParent = func(id snowflake.ID) snowflake.ID {
		if th, ok := client.Caches.GuildThread(id); ok && th.ParentID() != nil {
			return *th.ParentID()
		}
		return 0
	}
	client.AddEventListeners(
		bot.NewListenerFunc(discordio.GuildMessageHandler(ctx, r, client.Rest, &discordio.ChannelTyping{Sender: client.Rest, Logger: logger})),
		// Slash commands: same router, same access policy as mentions.
		bot.NewListenerFunc(discordio.SlashCommandHandler(ctx, r, client.Rest, cfg.UnauthorizedMessage, logger)),
		// Removed from voice by someone else: discard that server's queue.
		// A question deleted in Discord is dropped from ask's memory too.
		bot.NewListenerFunc(discordio.MessageDeleteHandler(memory.ForgetMessage)),
		bot.NewListenerFunc(discordio.BotVoiceLeaveHandler(client.ID, func(guildID snowflake.ID) { q.Disconnected(guildID) })),
	)

	// Replace the registered slash commands with this build's list, so Discord
	// always matches the code. If it fails, mentions still work.
	if registered, err := client.Rest.SetGlobalCommands(client.ApplicationID, discordio.SlashCommands(grantable, askOn, askOn && cfg.LLMWebSearch == commands.SearchOnRequest)); err != nil {
		logger.Warn("couldn't register slash commands; @mentions still work", "err", err)
	} else {
		logger.Info("slash commands registered", "count", len(registered))
	}

	if err := client.OpenGateway(ctx); err != nil {
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) && closeErr.Code == gateway.CloseEventCodeDisallowedIntent.Code {
			return errors.New("Discord refused the Message Content intent (discord_message_content: true): turn it on in the Developer Portal > your app > Bot > Privileged Gateway Intents, or set discord_message_content: false")
		}
		return fmt.Errorf("connect to discord gateway: %w", err)
	}
	logger.Info("running; press Ctrl+C to stop")

	<-ctx.Done()
	logger.Info("shutting down")
	q.Wait()     // ctx is cancelled, so playback stops, voice is left, and downloads end
	audio.Wait() // and the cache's purge loop stops
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

// checkCookies warns early if yt-dlp's cookies file is missing or read-only
// (yt-dlp writes refreshed cookies back to it). Only the path is ever logged:
// the file is a login credential.
func checkCookies(logger *slog.Logger, path string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		logger.Error("ytdlp_cookies is set but the file is missing or not writable; downloads may be blocked", "path", path, "err", err)
		return
	}
	f.Close()
	logger.Info("yt-dlp cookies in use", "path", path)
}
