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

func TestSlashArgs(t *testing.T) {
	cases := map[string]string{
		playData("never gonna give you up"):         "never gonna give you up",
		playData("  https://youtu.be/dQw4w9WgXcQ "): "https://youtu.be/dQw4w9WgXcQ",
		playData("2"):                           "2",
		simpleData("queue"):                     "",
		simpleData("access"):                    "",
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
	for _, raw := range []string{simpleData("nope"), `{"id":"1","name":"allow","type":1}`} {
		if _, ok := slashArgs(slashData(t, raw)); ok {
			t.Errorf("%s: want not ok", raw)
		}
	}
}

// The slash definitions and the command registry name exactly the same commands.
func TestSlashCommandsMatchRegistry(t *testing.T) {
	reg := []commands.Command{
		commands.Ping{}, commands.Play{}, commands.Test{}, commands.QueueList{}, commands.Skip{}, commands.Stop{},
		commands.Allow{}, commands.Deny{}, commands.AccessList{},
	}
	var want []string
	for _, c := range reg {
		want = append(want, c.Name())
	}
	var got []string
	byName := map[string]discord.SlashCommandCreate{}
	for _, c := range SlashCommands([]string{"play"}) {
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
	}
}

// fakeEvent is a slash command interaction.
type fakeEvent struct {
	raw     discord.SlashCommandInteractionData
	guild   *snowflake.ID
	user    snowflake.ID
	log     *[]string // shared, ordered: respond/defer/update/followup/run
	private []discord.MessageCreate
}

func (f *fakeEvent) data() (discord.SlashCommandInteractionData, bool) { return f.raw, true }
func (f *fakeEvent) guildID() *snowflake.ID                            { return f.guild }
func (f *fakeEvent) channelID() snowflake.ID                           { return 1 }
func (f *fakeEvent) userID() snowflake.ID                              { return f.user }
func (f *fakeEvent) applicationID() snowflake.ID                       { return 1000 }
func (f *fakeEvent) token() string                                     { return "tok" }
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
	return nil, nil
}
func (f *fakeResponder) CreateFollowupMessage(app snowflake.ID, token string, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	*f.log = append(*f.log, "followup")
	f.followups = append(f.followups, m)
	return nil, nil
}

// recordCmd stands in for a playback command: it records its args and replies.
type recordCmd struct {
	name    string
	log     *[]string
	replies int
}

func (c recordCmd) Name() string { return c.name }
func (c recordCmd) Run(ctx context.Context, req commands.Request) error {
	*c.log = append(*c.log, "run "+c.name+" "+req.Args)
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
		Owners: []snowflake.ID{owner}, Public: []string{"ping"}, OwnerOnly: commands.ManagementCommands, Inherit: inherit,
	})
	var reg *commands.Registry
	known := func(name string) bool { _, ok := reg.Lookup(name); return ok }
	cmds := []commands.Command{
		commands.Ping{},
		commands.Allow{Access: policy, Known: known}, commands.Deny{Access: policy}, commands.AccessList{Access: policy},
		recordCmd{name: "play", log: &e.log, replies: 1},
	}
	for _, c := range commands.PlaybackCommands {
		cmds = append(cmds, recordCmd{name: c, log: &e.log, replies: 1})
	}
	reg, err := commands.NewRegistry(cmds...)
	if err != nil {
		t.Fatal(err)
	}
	e.router = router.New(func() snowflake.ID { return 1000 }, reg, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		{friend, otherG, "play x", playData("x"), false, false},          // grant, but server not allowed
		{friend, allowedG, "access", simpleData("access"), false, false}, // owner-only
		{friend, allowedG, "allow <@9>", userData("allow", stranger, ""), false, false},
		{stranger, allowedG, "play x", playData("x"), false, false},
		{owner, allowedG, "deny <@8>", userData("deny", friend, ""), true, true},
		{friend, allowedG, "play x", playData("x"), false, false}, // revoked
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
