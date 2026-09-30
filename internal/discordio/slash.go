package discordio

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/router"
)

// Slash commands are a second way in, next to "@Bot <command>". Each one is
// turned into the same router.Invocation a mention would produce (the options
// become the argument text a user would type), so both go through the same
// access policy and the same command code.

// guildOnly limits a command to servers (no DMs, like mentions).
var guildOnly = []discord.InteractionContextType{discord.InteractionContextTypeGuild}

// SlashCommands defines every command for Discord. grantable lists the commands
// offered by "/allow user" and "/deny user"; ask adds /ask (when an LLM is set
// up), and askSearch its "search" switch (when search is on request).
func SlashCommands(grantable []string, ask, askSearch bool) []discord.ApplicationCommandCreate {
	simple := func(name, desc string) discord.ApplicationCommandCreate {
		return discord.SlashCommandCreate{Name: name, Description: desc, Contexts: guildOnly}
	}
	choices := make([]discord.ApplicationCommandOptionChoiceString, 0, len(grantable))
	for _, c := range grantable {
		choices = append(choices, discord.ApplicationCommandOptionChoiceString{Name: c, Value: c})
	}
	manage := func(name, verb string) discord.ApplicationCommandCreate {
		subs := []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "guild",
				Description: verb + " a server (this one by default)",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionString{Name: "id", Description: "Server ID (default: this server)"},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "user",
				Description: verb + " a command for a user",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionUser{Name: "user", Description: "Who", Required: true},
					discord.ApplicationCommandOptionString{Name: "command", Description: "Which command (default: play)", Choices: choices},
				},
			},
		}
		desc := verb + " a server, or a command for a user (owners only)"
		if ask {
			desc = verb + " a server, a command for a user, or an ask channel (owners only)"
			subs = append(subs, discord.ApplicationCommandOptionSubCommand{
				Name:        commands.AskCommand,
				Description: verb + " a channel for ask (this one by default)",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionChannel{Name: "channel", Description: "Channel in this server",
						ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText, discord.ChannelTypeGuildNews}},
					discord.ApplicationCommandOptionString{Name: "id", Description: "Channel ID, for another server"},
				},
			})
		}
		return discord.SlashCommandCreate{
			Name:        name,
			Description: desc,
			Contexts:    guildOnly,
			Options:     subs,
		}
	}
	all := []discord.ApplicationCommandCreate{
		simple("ping", "Check that the bot is listening"),
		discord.SlashCommandCreate{
			Name:        "play",
			Description: "Play a YouTube or Spotify link, search YouTube, or pick a search result by number",
			Contexts:    guildOnly,
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "query", Description: "YouTube link, Spotify link, search term, or search result number", Required: true},
				discord.ApplicationCommandOptionBool{Name: "lucky", Description: "For a search term: play the first good result instead of listing them"},
			},
		},
		simple("test", "Play a short test tone"),
		simple("queue", "Show this server's queue"),
		simple("skip", "Skip the current track"),
		simple("stop", "Clear the queue and leave voice"),
		manage("allow", "Allow"),
		manage("deny", "Remove"),
	}
	if ask {
		all = append(all, simple(commands.ForgetCommand, "Make the bot forget this channel's conversation"))
		askOpts := []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{Name: "prompt", Description: "Your question or request", Required: true},
		}
		if askSearch {
			askOpts = append(askOpts, discord.ApplicationCommandOptionBool{Name: "search", Description: "Search the web for this one (for current info; uses more of your daily limit)"})
		}
		all = append(all, discord.SlashCommandCreate{
			Name:        commands.AskCommand,
			Description: "Ask the bot anything",
			Contexts:    guildOnly,
			Options:     askOpts,
		})
	}
	return all
}

// slashArgs turns a slash command's options into the argument text of the
// equivalent mention command, e.g. "/allow user user:@x command:play" into
// "<@x> play". ok is false for a command this bot doesn't define.
func slashArgs(data discord.SlashCommandInteractionData) (args string, ok bool) {
	opt := func(name string) string {
		s, _ := data.OptString(name)
		return strings.TrimSpace(s)
	}
	switch name := data.CommandName(); name {
	case "ping", "test", "queue", "skip", "stop", commands.ForgetCommand:
		return "", true
	case "play":
		return opt("query"), true
	case commands.AskCommand:
		return opt("prompt"), true
	case "allow", "deny":
		if data.SubCommandName == nil {
			return "", false
		}
		switch *data.SubCommandName {
		case "guild":
			return strings.TrimSpace("guild " + opt("id")), true
		case "user":
			user, found := data.OptSnowflake("user")
			if !found {
				return "", false
			}
			return strings.TrimSpace(commands.Mention(user) + " " + opt("command")), true
		case commands.AskCommand:
			// The picked channel wins over a typed ID; neither means this channel.
			if ch, found := data.OptSnowflake("channel"); found {
				return commands.AskCommand + " " + ch.String(), true
			}
			return strings.TrimSpace(commands.AskCommand + " " + opt("id")), true
		}
	}
	return "", false
}

// InteractionResponder is the subset of disgo's REST client used to answer
// slash commands.
type InteractionResponder interface {
	UpdateInteractionResponse(applicationID snowflake.ID, interactionToken string, messageUpdate discord.MessageUpdate, opts ...rest.RequestOpt) (*discord.Message, error)
	CreateFollowupMessage(applicationID snowflake.ID, interactionToken string, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
	DeleteInteractionResponse(applicationID snowflake.ID, interactionToken string, opts ...rest.RequestOpt) error
}

// InteractionReplier answers a deferred slash command: the first reply
// replaces the "thinking…" message, later ones are follow-ups. A private
// reply (r.Private) is a follow-up only the user sees; as a first reply it
// removes the public "thinking…" (Discord can't make that private). Like
// ChannelReplier, it pings only r.Mentions.
type InteractionReplier struct {
	Rest          InteractionResponder
	ApplicationID snowflake.ID
	Token         string

	mu   sync.Mutex
	used bool
}

func (i *InteractionReplier) Reply(ctx context.Context, r commands.Reply) error {
	i.mu.Lock()
	first := !i.used
	i.used = true
	i.mu.Unlock()
	mentions := allowedMentions(r.Mentions)
	if r.Private {
		if first {
			if err := i.Rest.DeleteInteractionResponse(i.ApplicationID, i.Token, rest.WithCtx(ctx)); err != nil {
				return err
			}
		}
		_, err := i.Rest.CreateFollowupMessage(i.ApplicationID, i.Token,
			discord.MessageCreate{Content: r.Content, AllowedMentions: mentions, Flags: discord.MessageFlagEphemeral}, rest.WithCtx(ctx))
		return err
	}
	var sent *discord.Message
	var err error
	if first {
		sent, err = i.Rest.UpdateInteractionResponse(i.ApplicationID, i.Token,
			discord.MessageUpdate{Content: &r.Content, AllowedMentions: mentions}, rest.WithCtx(ctx))
	} else {
		sent, err = i.Rest.CreateFollowupMessage(i.ApplicationID, i.Token,
			discord.MessageCreate{Content: r.Content, AllowedMentions: mentions}, rest.WithCtx(ctx))
	}
	if err == nil {
		reportSent(r, sent)
	}
	return err
}

// Used reports whether anything was replied.
func (i *InteractionReplier) Used() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.used
}

// allowedMentions pings only users (explicit empty lists: no @everyone/@here/roles).
func allowedMentions(users []snowflake.ID) *discord.AllowedMentions {
	if users == nil {
		users = []snowflake.ID{}
	}
	return &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: users}
}

// slashEvent is what the handler needs from a slash command interaction
// (events.ApplicationCommandInteractionCreate implements it via slashAdapter).
type slashEvent interface {
	data() (discord.SlashCommandInteractionData, bool)
	guildID() *snowflake.ID
	channelID() snowflake.ID
	userID() snowflake.ID
	userName() string
	applicationID() snowflake.ID
	token() string
	respond(discord.MessageCreate) error // an immediate reply
	deferReply() error                   // "thinking…", replaced later
}

// SlashCommandHandler returns a disgo listener for slash commands. A user the
// policy refuses gets unauthorized, visible only to them (Discord needs some
// reply; mentions from such users still get silence). base is cancelled on
// shutdown.
func SlashCommandHandler(base context.Context, r *router.Router, responder InteractionResponder, unauthorized string, logger *slog.Logger) func(*events.ApplicationCommandInteractionCreate) {
	return func(e *events.ApplicationCommandInteractionCreate) {
		handleSlash(base, r, responder, unauthorized, logger, slashAdapter{e})
	}
}

func handleSlash(base context.Context, r *router.Router, responder InteractionResponder, unauthorized string, logger *slog.Logger, e slashEvent) {
	private := func(content string) {
		if err := e.respond(discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral, AllowedMentions: allowedMentions(nil)}); err != nil {
			logger.Error("slash: reply failed", "err", err)
		}
	}
	data, ok := e.data()
	if !ok {
		return // not a slash command (e.g. a context-menu command); none are registered
	}
	guildID := e.guildID()
	if guildID == nil {
		private("Use this in a server.")
		return
	}
	args, ok := slashArgs(data)
	if !ok {
		private("unknown command")
		return
	}
	inv := router.Invocation{GuildID: *guildID, ChannelID: e.channelID(), AuthorID: e.userID(), AuthorName: e.userName(), Name: data.CommandName(), Args: args}
	switch inv.Name {
	case "play":
		inv.Lucky, _ = data.OptBool("lucky")
	case commands.AskCommand:
		inv.Search, _ = data.OptBool("search")
	}

	ctx, cancel := context.WithTimeout(base, HandleTimeout)
	defer cancel()
	switch r.Check(ctx, inv) {
	case router.Denied:
		private(unauthorized)
		return
	case router.WrongChannel:
		private(router.WrongChannelMessage(inv.Name))
		return
	}
	// Answer within Discord's 3 s; searches and Spotify lookups take longer.
	if err := e.deferReply(); err != nil {
		logger.Error("slash: couldn't acknowledge", "command", inv.Name, "err", err)
		return
	}
	rep := &InteractionReplier{Rest: responder, ApplicationID: e.applicationID(), Token: e.token()}
	r.Run(ctx, inv, rep)
	if !rep.Used() {
		// Never leave "thinking…" forever.
		if err := rep.Reply(ctx, commands.Reply{Content: "Done."}); err != nil {
			logger.Error("slash: reply failed", "err", err)
		}
	}
}

type slashAdapter struct {
	e *events.ApplicationCommandInteractionCreate
}

func (a slashAdapter) data() (discord.SlashCommandInteractionData, bool) {
	d, ok := a.e.Data.(discord.SlashCommandInteractionData)
	return d, ok
}
func (a slashAdapter) guildID() *snowflake.ID { return a.e.GuildID() }
func (a slashAdapter) channelID() snowflake.ID {
	if ch := a.e.Channel(); ch.MessageChannel != nil {
		return ch.ID()
	}
	return 0
}
func (a slashAdapter) userID() snowflake.ID { return a.e.User().ID }
func (a slashAdapter) userName() string {
	if m := a.e.Member(); m != nil {
		return m.EffectiveName()
	}
	return a.e.User().EffectiveName()
}
func (a slashAdapter) applicationID() snowflake.ID           { return a.e.ApplicationID() }
func (a slashAdapter) token() string                         { return a.e.Token() }
func (a slashAdapter) respond(m discord.MessageCreate) error { return a.e.CreateMessage(m) }
func (a slashAdapter) deferReply() error                     { return a.e.DeferCreateMessage(false) }
