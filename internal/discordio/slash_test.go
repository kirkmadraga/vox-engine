package discordio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/router"
)

// slashData decodes the "data" object of a real slash command interaction.
func slashData(t *testing.T, raw string) discord.SlashCommandInteractionData {
	t.Helper()
	var d discord.SlashCommandInteractionData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return d
}

// Payload shapes as Discord sends them (option types: 1 subcommand, 3 string, 6 user).
func playData(query string) string {
	q, _ := json.Marshal(query)
	return `{"id":"1","name":"play","type":1,"options":[{"name":"query","type":3,"value":` + string(q) + `}]}`
}

func askData(prompt string) string {
	p, _ := json.Marshal(prompt)
	return `{"id":"1","name":"ask","type":1,"options":[{"name":"prompt","type":3,"value":` + string(p) + `}]}`
}

func simpleData(name string) string { return `{"id":"1","name":"` + name + `","type":1}` }

func userData(verb string, user snowflake.ID, command string) string {
	opts := `{"name":"user","type":6,"value":"` + user.String() + `"}`
	if command != "" {
		opts += `,{"name":"command","type":3,"value":"` + command + `"}`
	}
	return `{"id":"1","name":"` + verb + `","type":1,"options":[{"name":"user","type":1,"options":[` + opts + `]}],` +
		`"resolved":{"users":{"` + user.String() + `":{"id":"` + user.String() + `","username":"x"}}}}`
}

func guildData(verb, id string) string {
	opts := ""
	if id != "" {
		opts = `{"name":"id","type":3,"value":"` + id + `"}`
	}
	return `{"id":"1","name":"` + verb + `","type":1,"options":[{"name":"guild","type":1,"options":[` + opts + `]}]}`
}

// openData is "/allow everyone" or "/deny public" etc., with an optional command.
func openData(verb, scope, command string) string {
	opts := ""
	if command != "" {
		opts = `{"name":"command","type":3,"value":"` + command + `"}`
	}
	return `{"id":"1","name":"` + verb + `","type":1,"options":[{"name":"` + scope + `","type":1,"options":[` + opts + `]}]}`
}

func TestSlashArgs(t *testing.T) {
	cases := map[string]string{
		openData("allow", "everyone", ""):           "everyone",
		openData("allow", "everyone", "remindme"):   "everyone remindme",
		openData("deny", "public", "play"):          "public play",
		openData("deny", "everyone", "ask"):         "everyone ask",
		openData("allow", "public", ""):             "public",
		playData("never gonna give you up"):         "never gonna give you up",
		playData("  https://youtu.be/dQw4w9WgXcQ "): "https://youtu.be/dQw4w9WgXcQ",
		playData("2"):                           "2",
		askData("  What is jazz? "):             "What is jazz?",
		simpleData("queue"):                     "",
		simpleData("forget"):                    "",
		userData("allow", 8, "play"):            "<@8> play",
		userData("deny", 8, ""):                 "<@8>",
		guildData("allow", ""):                  "guild",
		guildData("deny", "123456789012345678"): "guild 123456789012345678",
	}
	for raw, want := range cases {
		if got, ok := slashArgs(slashData(t, raw)); !ok || got != want {
			t.Errorf("%s: args = %q, %v; want %q", raw, got, ok, want)
		}
	}
	// "/allow ask" (removed in v1.1.0) from a stale client is unknown, not run.
	staleAsk := `{"id":"1","name":"allow","type":1,"options":[{"name":"ask","type":1,"options":[]}]}`
	for _, raw := range []string{simpleData("nope"), `{"id":"1","name":"allow","type":1}`, staleAsk} {
		if _, ok := slashArgs(slashData(t, raw)); ok {
			t.Errorf("%s: want not ok", raw)
		}
	}
}

// The slash definitions and the command registry name exactly the same commands.
func TestSlashCommandsMatchRegistry(t *testing.T) {
	reg := []commands.Command{
		commands.Ping{}, commands.Play{}, commands.Test{}, commands.QueueList{}, commands.Skip{}, commands.Stop{},
		commands.Allow{}, commands.Deny{}, &commands.Ask{}, commands.Forget{}, commands.RemindMe{}, // debug: hidden, mentions only
	}
	var want []string
	for _, c := range reg {
		want = append(want, c.Name())
	}
	var got []string
	byName := map[string]discord.SlashCommandCreate{}
	for _, c := range SlashCommands([]string{"play"}, true, true) {
		s := c.(discord.SlashCommandCreate)
		got = append(got, s.Name)
		byName[s.Name] = s
		if !slices.Equal(s.Contexts, []discord.InteractionContextType{discord.InteractionContextTypeGuild}) {
			t.Errorf("/%s must be server-only", s.Name)
		}
		if s.Description == "" || len(s.Description) > 100 {
			t.Errorf("/%s: description must be 1-100 characters, got %d", s.Name, len(s.Description))
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("slash commands %q, registry %q", got, want)
	}
	if q := byName["play"].Options[0].(discord.ApplicationCommandOptionString); q.Name != "query" || !q.Required {
		t.Errorf("/play needs a required query: %+v", q)
	}
	for _, verb := range []string{"allow", "deny"} {
		user := byName[verb].Options[1].(discord.ApplicationCommandOptionSubCommand)
		if u := user.Options[0].(discord.ApplicationCommandOptionUser); !u.Required {
			t.Errorf("/%s user: the user must be required", verb)
		}
		cmd := user.Options[1].(discord.ApplicationCommandOptionString)
		if len(cmd.Choices) != 1 || cmd.Choices[0].Value != "play" {
			t.Errorf("/%s user command choices = %+v", verb, cmd.Choices)
		}
		var subs []string
		for i, o := range byName[verb].Options {
			sub := o.(discord.ApplicationCommandOptionSubCommand)
			subs = append(subs, sub.Name)
			if len(sub.Description) == 0 || len(sub.Description) > 100 {
				t.Errorf("/%s %s: description must be 1-100 characters, got %d", verb, sub.Name, len(sub.Description))
			}
			if i < 2 {
				continue
			}
			// everyone and public: just the command, optional, same choices.
			if len(sub.Options) != 1 {
				t.Fatalf("/%s %s options = %+v", verb, sub.Name, sub.Options)
			}
			c := sub.Options[0].(discord.ApplicationCommandOptionString)
			if c.Name != "command" || c.Required || len(c.Choices) != 1 || c.Choices[0].Value != "play" {
				t.Errorf("/%s %s command = %+v", verb, sub.Name, c)
			}
		}
		if !slices.Equal(subs, []string{"guild", "user", "everyone", "public"}) {
			t.Errorf("/%s subcommands = %q", verb, subs)
		}
	}
}

// fakeEvent is a slash command interaction.
type fakeEvent struct {
	raw     discord.SlashCommandInteractionData
	guild   *snowflake.ID
	user    snowflake.ID
	channel snowflake.ID // 0 = channel 1
	log     *[]string    // shared, ordered: respond/defer/update/followup/run
	private []discord.MessageCreate
}

func (f *fakeEvent) data() (discord.SlashCommandInteractionData, bool) { return f.raw, true }
func (f *fakeEvent) guildID() *snowflake.ID                            { return f.guild }
func (f *fakeEvent) channelID() snowflake.ID {
	if f.channel == 0 {
		return 1
	}
	return f.channel
}
func (f *fakeEvent) userID() snowflake.ID        { return f.user }
func (f *fakeEvent) userName() string            { return "user" + f.user.String() }
func (f *fakeEvent) applicationID() snowflake.ID { return 1000 }
func (f *fakeEvent) token() string               { return "tok" }
func (f *fakeEvent) respond(m discord.MessageCreate) error {
	f.private = append(f.private, m)
	*f.log = append(*f.log, "respond")
	return nil
}
func (f *fakeEvent) deferReply() error { *f.log = append(*f.log, "defer"); return nil }

type fakeResponder struct {
	log       *[]string
	updates   []discord.MessageUpdate
	followups []discord.MessageCreate
}

func (f *fakeResponder) UpdateInteractionResponse(app snowflake.ID, token string, u discord.MessageUpdate, _ ...rest.RequestOpt) (*discord.Message, error) {
	*f.log = append(*f.log, "update")
	f.updates = append(f.updates, u)
	return &discord.Message{ID: 500}, nil
}
func (f *fakeResponder) DeleteInteractionResponse(app snowflake.ID, token string, _ ...rest.RequestOpt) error {
	*f.log = append(*f.log, "delete")
	return nil
}
func (f *fakeResponder) CreateFollowupMessage(app snowflake.ID, token string, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	*f.log = append(*f.log, "followup")
	f.followups = append(f.followups, m)
	return &discord.Message{ID: snowflake.ID(600 + len(f.followups))}, nil
}

// recordCmd stands in for a playback command: it records its args and replies.
type recordCmd struct {
	name    string
	log     *[]string
	replies int
}

func (c recordCmd) Name() string { return c.name }
func (c recordCmd) Run(ctx context.Context, req commands.Request) error {
	entry := "run " + c.name + " " + req.Args
	if req.Lucky {
		entry += " [lucky]"
	}
	if req.Search {
		entry += " [search]"
	}
	if req.When != "" {
		entry += " [when " + req.When + "]"
	}
	*c.log = append(*c.log, entry)
	for i := range c.replies {
		req.Reply.Reply(ctx, commands.Reply{Content: fmt.Sprintf("%s reply %d @everyone", c.name, i+1)})
	}
	return nil
}

const (
	owner    snowflake.ID = 7
	friend   snowflake.ID = 8
	stranger snowflake.ID = 9
	allowedG snowflake.ID = 100
	otherG   snowflake.ID = 200
)

const denied = "Ask the owner for access."

type slashE2E struct {
	t      *testing.T
	router *router.Router
	log    []string
	mu     sync.Mutex
}

// newSlashE2E wires the real access Policy (in memory) and the real management
// commands, like main does; playback commands are recorders.
func newSlashE2E(t *testing.T) *slashE2E {
	e := &slashE2E{t: t}
	inherit := map[string]string{}
	for _, c := range commands.PlaybackCommands {
		inherit[c] = "play"
	}
	policy := access.NewPolicy(accesstest.NewMemory(), access.Options{
		Owners: []snowflake.ID{owner}, AlwaysOpen: []string{"ping"}, OwnerOnly: commands.ManagementCommands, Inherit: inherit,
	})
	var reg *commands.Registry
	known := func(name string) bool { _, ok := reg.Lookup(name); return ok }
	cmds := []commands.Command{
		commands.Ping{},
		commands.Allow{Access: policy, Known: known}, commands.Deny{Access: policy}, commands.Debug{Access: policy},
		recordCmd{name: "play", log: &e.log, replies: 1},
		recordCmd{name: commands.AskCommand, log: &e.log, replies: 1},
		recordCmd{name: commands.RemindCommand, log: &e.log, replies: 1},
	}
	for _, c := range commands.PlaybackCommands {
		cmds = append(cmds, recordCmd{name: c, log: &e.log, replies: 1})
	}
	reg, err := commands.NewRegistry(cmds...)
	if err != nil {
		t.Fatal(err)
	}
	e.router = router.New(func() snowflake.ID { return 1000 }, reg, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.router.Fallback = commands.AskCommand
	return e
}

// slash sends a slash command and returns the event (private replies) and responder (normal replies).
func (e *slashE2E) slash(user, guild snowflake.ID, raw string) (*fakeEvent, *fakeResponder) {
	e.t.Helper()
	e.log = nil
	ev := &fakeEvent{raw: slashData(e.t, raw), guild: &guild, user: user, log: &e.log}
	resp := &fakeResponder{log: &e.log}
	handleSlash(context.Background(), e.router, resp, denied, slog.New(slog.NewTextHandler(io.Discard, nil)), ev)
	return ev, resp
}

type mentionReplier struct{ got []commands.Reply }

func (m *mentionReplier) Reply(_ context.Context, r commands.Reply) error {
	m.got = append(m.got, r)
	return nil
}

// both runs the same command as a mention and as a slash command and checks
// they agree: a mention reply means the slash command ran; mention silence
// means the private "not allowed" reply. It returns whether it was allowed.
func (e *slashE2E) both(user, guild snowflake.ID, mention, raw string) bool {
	e.t.Helper()
	mr := &mentionReplier{}
	e.router.Handle(context.Background(), router.Message{GuildID: guild, ChannelID: 1, AuthorID: user, Content: "<@1000> " + mention}, mr)
	mentionAllowed := len(mr.got) > 0

	ev, resp := e.slash(user, guild, raw)
	slashDenied := len(ev.private) == 1 && ev.private[0].Content == denied
	slashRan := len(resp.updates) > 0
	switch {
	case mentionAllowed && !slashRan, !mentionAllowed && !slashDenied:
		e.t.Errorf("user %d in guild %d, %q: mention allowed=%v, slash ran=%v denied=%v", user, guild, mention, mentionAllowed, slashRan, slashDenied)
	}
	return mentionAllowed
}

// The same policy decides both ways in, step by step through a realistic setup.
func TestSlashAndMentionsShareThePolicy(t *testing.T) {
	e := newSlashE2E(t)
	steps := []struct {
		user, guild   snowflake.ID
		mention, raw  string
		want          bool
		changesPolicy bool
	}{
		{stranger, otherG, "ping", simpleData("ping"), false, false}, // server not allowed
		{owner, otherG, "ping", simpleData("ping"), true, false},     // owners: everywhere
		{friend, allowedG, "ping", simpleData("ping"), false, false}, // not allowed yet
		{owner, allowedG, "allow guild", guildData("allow", ""), true, true},
		{friend, allowedG, "ping", simpleData("ping"), true, false},    // public, allowed server
		{friend, allowedG, "play x", playData("x"), false, false},      // no grant yet
		{friend, allowedG, "queue", simpleData("queue"), false, false}, // inherits play: no
		{owner, allowedG, "allow <@8> play", userData("allow", friend, "play"), true, true},
		{friend, allowedG, "play x", playData("x"), true, false},
		{friend, allowedG, "queue", simpleData("queue"), true, false}, // inherits play
		{friend, allowedG, "stop", simpleData("stop"), true, false},
		{friend, allowedG, "what is jazz", askData("what is jazz"), false, false}, // play's grant doesn't cover ask
		{friend, allowedG, "ask what is jazz", askData("what is jazz"), false, false},
		{owner, allowedG, "allow <@8> ask", userData("allow", friend, "ask"), true, true},
		{friend, allowedG, "what is jazz", askData("what is jazz"), true, false},
		{friend, allowedG, "ask what is jazz", askData("what is jazz"), true, false},
		{stranger, allowedG, "what is jazz", askData("what is jazz"), false, false},
		{friend, otherG, "play x", playData("x"), false, false}, // grant, but server not allowed
		{friend, allowedG, "allow <@9>", userData("allow", stranger, ""), false, false},
		{stranger, allowedG, "play x", playData("x"), false, false},
		{owner, allowedG, "deny <@8>", userData("deny", friend, ""), true, true},
		{friend, allowedG, "play x", playData("x"), false, false}, // revoked
		// Everyone in this server, then everyone everywhere.
		{stranger, allowedG, "allow everyone", openData("allow", "everyone", ""), false, false}, // owners only
		{owner, allowedG, "allow everyone", openData("allow", "everyone", ""), true, true},
		{stranger, allowedG, "play x", playData("x"), true, false},
		{stranger, allowedG, "queue", simpleData("queue"), true, false}, // inherits play
		{friend, allowedG, "play x", playData("x"), true, false},        // no grant needed
		{stranger, otherG, "play x", playData("x"), false, false},       // only that server
		{stranger, allowedG, "what is jazz", askData("what is jazz"), false, false},
		{owner, allowedG, "deny everyone play", openData("deny", "everyone", "play"), true, true},
		{stranger, allowedG, "play x", playData("x"), false, false},
		{stranger, allowedG, "allow public play", openData("allow", "public", "play"), false, false}, // owners only
		{owner, allowedG, "allow public play", openData("allow", "public", "play"), true, true},
		{stranger, otherG, "play x", playData("x"), true, false}, // any server, allowed or not
		{stranger, otherG, "stop", simpleData("stop"), true, false},
		{stranger, otherG, "ping", simpleData("ping"), false, false}, // ping still needs an allowed server
		{stranger, allowedG, "play x", playData("x"), true, false},
		{owner, allowedG, "deny public", openData("deny", "public", ""), true, true},
		{stranger, otherG, "play x", playData("x"), false, false},
		{stranger, allowedG, "play x", playData("x"), false, false},
	}
	for i, s := range steps {
		if s.changesPolicy {
			// Run policy changes once (as a mention), then check the slash form is allowed too.
			mr := &mentionReplier{}
			e.router.Handle(context.Background(), router.Message{GuildID: s.guild, ChannelID: 1, AuthorID: s.user, Content: "<@1000> " + s.mention}, mr)
			if len(mr.got) != 1 {
				t.Fatalf("step %d %q: %+v", i, s.mention, mr.got)
			}
			if ev, _ := e.slash(s.user, s.guild, s.raw); len(ev.private) != 0 {
				t.Errorf("step %d: owner's slash %q was refused", i, s.mention)
			}
			continue
		}
		if got := e.both(s.user, s.guild, s.mention, s.raw); got != s.want {
			t.Errorf("step %d: user %d in guild %d %q allowed = %v, want %v", i, s.user, s.guild, s.mention, got, s.want)
		}
	}
}

func TestSlashDeniedIsPrivateAndNothingRuns(t *testing.T) {
	e := newSlashE2E(t)
	ev, resp := e.slash(stranger, otherG, playData("x"))
	if len(ev.private) != 1 || ev.private[0].Content != denied || ev.private[0].Flags&discord.MessageFlagEphemeral == 0 {
		t.Fatalf("private replies = %+v", ev.private)
	}
	if m := ev.private[0].AllowedMentions; m == nil || len(m.Parse) != 0 || len(m.Users) != 0 {
		t.Errorf("the denial must ping no one: %+v", m)
	}
	if !slices.Equal(e.log, []string{"respond"}) || len(resp.updates) != 0 {
		t.Errorf("events = %q, updates = %+v", e.log, resp.updates)
	}
}

// Allowed: "thinking…" first, then the command, whose replies edit it and follow up.
func TestSlashDefersThenReplies(t *testing.T) {
	e := newSlashE2E(t)
	ev, resp := e.slash(owner, allowedG, playData("never gonna"))
	want := []string{"defer", "run play never gonna", "update"}
	if !slices.Equal(e.log, want) || len(ev.private) != 0 {
		t.Fatalf("events = %q, want %q (private %+v)", e.log, want, ev.private)
	}
	u := resp.updates[0]
	if u.Content == nil || *u.Content != "play reply 1 @everyone" {
		t.Errorf("update = %+v", u)
	}
	if m := u.AllowedMentions; m == nil || len(m.Parse) != 0 || len(m.Roles) != 0 || len(m.Users) != 0 {
		t.Errorf("replies must never ping @everyone/roles: %+v", m)
	}
}

func TestInteractionReplierFollowsUp(t *testing.T) {
	var log []string
	resp := &fakeResponder{log: &log}
	r := &InteractionReplier{Rest: resp, ApplicationID: 1000, Token: "tok"}
	r.Reply(context.Background(), commands.Reply{Content: "first"})
	r.Reply(context.Background(), commands.Reply{Content: "second", Mentions: []snowflake.ID{8}})
	if !slices.Equal(log, []string{"update", "followup"}) || resp.followups[0].Content != "second" {
		t.Fatalf("log = %q, followups = %+v", log, resp.followups)
	}
	if m := resp.followups[0].AllowedMentions; !slices.Equal(m.Users, []snowflake.ID{8}) {
		t.Errorf("follow-up must ping exactly the listed users: %+v", m)
	}
}

// Slash answers report their message IDs too (the edited "thinking…", then
// follow-ups), so replies to /ask answers continue the conversation.
func TestInteractionReplierReportsSentIDs(t *testing.T) {
	var log []string
	resp := &fakeResponder{log: &log}
	r := &InteractionReplier{Rest: resp, ApplicationID: 1000, Token: "tok"}
	var got []snowflake.ID
	sent := func(id snowflake.ID) { got = append(got, id) }
	r.Reply(context.Background(), commands.Reply{Content: "first", Sent: sent})
	r.Reply(context.Background(), commands.Reply{Content: "second", Sent: sent})
	if len(got) != 2 || got[0] != 500 {
		t.Errorf("sent IDs %v", got)
	}
}

// A private first reply removes the public "thinking…" and follows up
// privately; a private later reply is a private follow-up.
func TestInteractionReplierPrivate(t *testing.T) {
	var log []string
	resp := &fakeResponder{log: &log}
	r := &InteractionReplier{Rest: resp, ApplicationID: 1000, Token: "tok"}
	r.Reply(context.Background(), commands.Reply{Content: "You can ask again in 4s.", Private: true})
	r.Reply(context.Background(), commands.Reply{Content: "later", Private: true})
	if !slices.Equal(log, []string{"delete", "followup", "followup"}) {
		t.Fatalf("log = %q", log)
	}
	for _, f := range resp.followups {
		if f.Flags != discord.MessageFlagEphemeral {
			t.Errorf("%q: flags %v, want ephemeral", f.Content, f.Flags)
		}
	}
	if len(resp.updates) != 0 {
		t.Errorf("a private reply must not edit the public message: %+v", resp.updates)
	}
	if !r.Used() {
		t.Error("a private reply counts as replying (no extra \"Done.\")")
	}
}

// A command that never replies still replaces "thinking…".
func TestSlashSilentCommandSaysDone(t *testing.T) {
	e := newSlashE2E(t)
	reg, _ := commands.NewRegistry(recordCmd{name: "ping", log: &e.log, replies: 0})
	e.router = router.New(func() snowflake.ID { return 1000 }, reg, access.NewPolicy(accesstest.NewMemory(), access.Options{Owners: []snowflake.ID{owner}}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, resp := e.slash(owner, allowedG, simpleData("ping"))
	if len(resp.updates) != 1 || *resp.updates[0].Content != "Done." {
		t.Errorf("updates = %+v", resp.updates)
	}
}

func TestSlashOutsideServersAndUnknown(t *testing.T) {
	e := newSlashE2E(t)
	e.log = nil
	dm := &fakeEvent{raw: slashData(t, simpleData("ping")), user: owner, log: &e.log}
	handleSlash(context.Background(), e.router, &fakeResponder{log: &e.log}, denied, slog.New(slog.NewTextHandler(io.Discard, nil)), dm)
	if len(dm.private) != 1 || !strings.Contains(dm.private[0].Content, "in a server") {
		t.Errorf("DM: %+v", dm.private)
	}
	ev, _ := e.slash(owner, allowedG, simpleData("nope"))
	if len(ev.private) != 1 || ev.private[0].Content != "unknown command" {
		t.Errorf("unknown: %+v", ev.private)
	}
}

func luckyPlayData(query string, lucky bool) string {
	q, _ := json.Marshal(query)
	return fmt.Sprintf(`{"id":"1","name":"play","type":1,"options":[{"name":"query","type":3,"value":%s},{"name":"lucky","type":5,"value":%v}]}`, q, lucky)
}

// /play's lucky option reaches the command; leaving it out, or a mention,
// never sets it (mentions have no such option, and "--lucky" is just text).
func TestLuckyOnlyFromTheSlashOption(t *testing.T) {
	e := newSlashE2E(t)
	cases := map[string]string{
		luckyPlayData("stay beautiful", true):  "run play stay beautiful [lucky]",
		luckyPlayData("stay beautiful", false): "run play stay beautiful",
		playData("stay beautiful"):             "run play stay beautiful",
	}
	for raw, want := range cases {
		e.slash(owner, allowedG, raw)
		if !slices.Contains(e.log, want) {
			t.Errorf("%s: events %q, want %q", raw, e.log, want)
		}
	}
	e.log = nil
	e.router.Handle(context.Background(), router.Message{GuildID: allowedG, ChannelID: 1, AuthorID: owner, Content: "<@1000> play --lucky stay beautiful"}, &mentionReplier{})
	if !slices.Equal(e.log, []string{"run play --lucky stay beautiful"}) {
		t.Errorf("mention: events %q; a mention must never be lucky", e.log)
	}
	// Only /play reads it.
	e.slash(owner, allowedG, `{"id":"1","name":"queue","type":1,"options":[{"name":"lucky","type":5,"value":true}]}`)
	if !slices.Contains(e.log, "run queue ") {
		t.Errorf("queue: events %q", e.log)
	}
}

// /ask exists only when an LLM is set up, with a required prompt, and runs the
// ask command with the prompt as its args.
func TestSlashAsk(t *testing.T) {
	has := func(ask bool) (discord.SlashCommandCreate, bool) {
		for _, c := range SlashCommands([]string{"play"}, ask, ask) {
			if s := c.(discord.SlashCommandCreate); s.Name == "ask" {
				return s, true
			}
		}
		return discord.SlashCommandCreate{}, false
	}
	if _, ok := has(false); ok {
		t.Error("/ask is registered with no LLM set up")
	}
	for _, on := range []bool{false, true} {
		found := false
		for _, c := range SlashCommands([]string{"play"}, on, on) {
			found = found || c.(discord.SlashCommandCreate).Name == "forget"
		}
		if found != on {
			t.Errorf("ask on=%v: /forget registered = %v", on, found)
		}
	}
	s, ok := has(true)
	if !ok {
		t.Fatal("/ask is missing")
	}
	if p := s.Options[0].(discord.ApplicationCommandOptionString); p.Name != "prompt" || !p.Required {
		t.Errorf("/ask needs a required prompt: %+v", p)
	}

	e := newSlashE2E(t)
	e.slash(owner, allowedG, askData("  What is Jazz? "))
	if !slices.Contains(e.log, "run ask What is Jazz?") {
		t.Errorf("events %q", e.log)
	}
}

func TestPlayLuckyOptionIsOptional(t *testing.T) {
	for _, c := range SlashCommands([]string{"play"}, false, false) {
		s := c.(discord.SlashCommandCreate)
		if s.Name != "play" {
			continue
		}
		lucky, ok := s.Options[1].(discord.ApplicationCommandOptionBool)
		if !ok || lucky.Name != "lucky" || lucky.Required {
			t.Errorf("/play's second option = %+v; want an optional boolean named lucky", s.Options[1])
		}
	}
}

func remindData(sub, opts string) string {
	return `{"id":"1","name":"remindme","type":1,"options":[{"name":"` + sub + `","type":1,"options":[` + opts + `]}]}`
}

// /remindme set|list|cancel reach the command as its mention forms, with set's
// "when" kept apart from the text; it needs the remindme grant.
func TestSlashRemindMe(t *testing.T) {
	e := newSlashE2E(t)
	set := remindData("set", `{"name":"when","type":3,"value":" in 2h "},{"name":"text","type":3,"value":"at last, stretch"}`)
	for raw, want := range map[string]string{
		set:                    "run remindme at last, stretch [when in 2h]",
		remindData("list", ""): "run remindme list",
		remindData("cancel", `{"name":"number","type":4,"value":12}`): "run remindme cancel 12",
	} {
		if ev, _ := e.slash(owner, allowedG, raw); len(ev.private) != 0 || !slices.Contains(e.log, want) {
			t.Errorf("%s: events %q, want %q", raw, e.log, want)
		}
	}
	if ev, _ := e.slash(friend, allowedG, set); len(ev.private) != 1 || ev.private[0].Content != denied {
		t.Errorf("without the grant: private %+v", ev.private)
	}
	for _, raw := range []string{`{"id":"1","name":"remindme","type":1}`, remindData("cancel", ""), remindData("nope", "")} {
		if _, ok := slashArgs(slashData(t, raw)); ok {
			t.Errorf("%s: want not ok", raw)
		}
	}
	// The definition: three subcommands; set's options are both required.
	for _, c := range SlashCommands([]string{"play"}, false, false) {
		s := c.(discord.SlashCommandCreate)
		if s.Name != commands.RemindCommand {
			continue
		}
		var subs []string
		for _, o := range s.Options {
			subs = append(subs, o.(discord.ApplicationCommandOptionSubCommand).Name)
		}
		setOpts := s.Options[0].(discord.ApplicationCommandOptionSubCommand).Options
		when := setOpts[0].(discord.ApplicationCommandOptionString)
		text := setOpts[1].(discord.ApplicationCommandOptionString)
		if !slices.Equal(subs, []string{"set", "list", "cancel"}) || !when.Required || !text.Required || *text.MaxLength != commands.DefaultReminderChars {
			t.Errorf("/remindme: subcommands %q, when %+v, text %+v", subs, when, text)
		}
		return
	}
	t.Error("no /remindme, even with ask off")
}

// /allow and /deny have "guild", "user", "everyone" and "public": no ask
// channels (removed in v1.1.0), with or without ask.
func TestAllowDenyHaveNoAskChannels(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, c := range SlashCommands([]string{"play"}, on, on) {
			s := c.(discord.SlashCommandCreate)
			if s.Name != "allow" && s.Name != "deny" {
				continue
			}
			var subs []string
			for _, o := range s.Options {
				subs = append(subs, o.(discord.ApplicationCommandOptionSubCommand).Name)
			}
			if !slices.Equal(subs, []string{"guild", "user", "everyone", "public"}) || strings.Contains(s.Description, "channel") {
				t.Errorf("/%s with ask=%v: subcommands %q, description %q", s.Name, on, subs, s.Description)
			}
		}
	}
}

// ask follows the grant in every channel, with no channel setup: /ask, an
// explicit "ask" and the mention fallback all run for a granted user anywhere
// (threads included), and someone without the grant is refused as for any
// command (privately on slash, silently on mentions).
func TestAskInEveryChannelBothWays(t *testing.T) {
	e := newSlashE2E(t)
	mention := func(user, channel snowflake.ID, text string) []commands.Reply {
		mr := &mentionReplier{}
		e.router.Handle(context.Background(), router.Message{GuildID: allowedG, ChannelID: channel, AuthorID: user, Content: "<@1000> " + text}, mr)
		return mr.got
	}
	slashIn := func(user, channel snowflake.ID, raw string) *fakeEvent {
		e.log = nil
		ev := &fakeEvent{raw: slashData(t, raw), guild: new(allowedG), user: user, channel: channel, log: &e.log}
		handleSlash(context.Background(), e.router, &fakeResponder{log: &e.log}, denied, slog.New(slog.NewTextHandler(io.Discard, nil)), ev)
		return ev
	}
	mention(owner, 1, "allow guild")
	mention(owner, 1, "allow <@8> ask")

	for _, ch := range []snowflake.ID{1, 2, 3} { // any channel; 3 could be a thread: no lookup needed
		if ev := slashIn(friend, ch, askData("hi")); len(ev.private) != 0 || !slices.Contains(e.log, "run ask hi") {
			t.Errorf("/ask in channel %d: private %+v, events %q", ch, ev.private, e.log)
		}
		if got := mention(friend, ch, "ask hi"); len(got) != 1 || got[0].Content == denied {
			t.Errorf("explicit ask in channel %d: %+v", ch, got)
		}
		if got := mention(friend, ch, "hi there"); len(got) != 1 {
			t.Errorf("the fallback in channel %d: %+v", ch, got)
		}
		if ev := slashIn(stranger, ch, askData("hi")); len(ev.private) != 1 || ev.private[0].Content != denied || slices.Contains(e.log, "run ask hi") {
			t.Errorf("no grant, /ask in channel %d: private %+v, events %q", ch, ev.private, e.log)
		}
		if got := mention(stranger, ch, "hi there"); len(got) != 0 {
			t.Errorf("no grant, mention in channel %d must stay silent: %+v", ch, got)
		}
	}
}

// /ask's search switch exists only when search is on request, is optional,
// and reaches the command.
func TestSlashAskSearchOption(t *testing.T) {
	find := func(askSearch bool) (discord.ApplicationCommandOptionBool, bool) {
		for _, c := range SlashCommands([]string{"play"}, true, askSearch) {
			s := c.(discord.SlashCommandCreate)
			if s.Name != "ask" {
				continue
			}
			for _, o := range s.Options {
				if b, ok := o.(discord.ApplicationCommandOptionBool); ok && b.Name == "search" {
					return b, true
				}
			}
		}
		return discord.ApplicationCommandOptionBool{}, false
	}
	if _, ok := find(false); ok {
		t.Error("search switch shown although search isn't on request")
	}
	if b, ok := find(true); !ok || b.Required {
		t.Errorf("search switch: %+v, found %v; want an optional switch", b, ok)
	}

	e := newSlashE2E(t)
	raw := `{"id":"1","name":"ask","type":1,"options":[{"name":"prompt","type":3,"value":"news"},{"name":"search","type":5,"value":true}]}`
	e.slash(owner, allowedG, raw)
	if !slices.Contains(e.log, "run ask news [search]") {
		t.Errorf("events %q", e.log)
	}
}
