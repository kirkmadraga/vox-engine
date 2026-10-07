package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
)

// These tests run the real Policy over the in-memory backend. They depend only
// on the access.Backend interface, so they stay unchanged if storage changes.

const (
	ownerID  snowflake.ID = 1
	friendID snowflake.ID = 2
	guildID  snowflake.ID = 100
)

type env struct {
	policy *access.Policy
	allow  Allow
	deny   Deny
	list   Debug // "debug access"
}

func newEnv(backend access.Backend) env {
	p := access.NewPolicy(backend, access.Options{
		Owners:     []snowflake.ID{ownerID},
		AlwaysOpen: []string{"ping"},
		OwnerOnly:  ManagementCommands,
		Now:        func() time.Time { return time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC) },
	})
	known := func(c string) bool { return c == "ping" || c == "play" || c == "allow" }
	return env{policy: p, allow: Allow{Access: p, Known: known}, deny: Deny{Access: p}, list: Debug{Access: p}}
}

// run executes cmd as the owner in guildID and returns the single reply.
func run(t *testing.T, cmd Command, args string) Reply {
	t.Helper()
	rep := &fakeReplier{}
	if err := cmd.Run(context.Background(), Request{GuildID: guildID, AuthorID: ownerID, Args: args, Reply: rep}); err != nil {
		t.Fatalf("Run(%q): %v", args, err)
	}
	if len(rep.got) != 1 {
		t.Fatalf("Run(%q): got %d replies, want 1", args, len(rep.got))
	}
	if len(rep.got[0].Mentions) != 0 {
		t.Errorf("Run(%q): management replies must not ping anyone, got %v", args, rep.got[0].Mentions)
	}
	return rep.got[0]
}

func contains(t *testing.T, r Reply, want string) {
	t.Helper()
	if !strings.Contains(r.Content, want) {
		t.Errorf("reply %q does not contain %q", r.Content, want)
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in   string
		want target
		ok   bool
	}{
		{"guild", target{guild: true}, true},
		{"GUILD", target{guild: true}, true},
		{"<@2>", target{user: 2, command: "play"}, true},
		{"<@!2>", target{user: 2, command: "play"}, true},
		{"<@2> Image", target{user: 2, command: "image"}, true},
		{"  <@2>   ping  ", target{user: 2, command: "ping"}, true},
		{"guild 1100000000000000003", target{guild: true, guildID: 1100000000000000003}, true},
		{"Guild  300", target{guild: true, guildID: 300}, true},
		{"", target{}, false},
		{"guild extra", target{}, false},
		{"guild 0", target{}, false},
		{"guild -5", target{}, false},
		{"guild 300 400", target{}, false},
		{"<@2> play extra", target{}, false},
		{"2", target{}, false},
		{"<@abc>", target{}, false},
		{"<@0>", target{}, false},
		{"<@&2> play", target{}, false}, // role, not user
		{"@friend", target{}, false},
		{"everyone", target{everyone: true, command: "play"}, true},
		{"EVERYONE Remindme", target{everyone: true, command: "remindme"}, true},
		{"public", target{public: true, command: "play"}, true},
		{"  Public   ask ", target{public: true, command: "ask"}, true},
		{"everyone play extra", target{}, false},
		{"public 300 play", target{}, false},
		{"@everyone play", target{}, false}, // Discord's @everyone isn't the keyword (and would ping)
		{"everyone's", target{}, false},
		// Channel rules for ask were removed in v1.1.0: no channel targets.
		{"ask", target{}, false},
		{"ask <#500>", target{}, false},
	}
	for _, c := range cases {
		got, ok := parseTarget(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseTarget(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The old "allow/deny ask #channel" form now just shows the usage, which no
// longer mentions channels.
func TestAllowAskChannelIsGone(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	for _, c := range []struct {
		cmd  Command
		args string
	}{{e.allow, "ask"}, {e.allow, "ask <#500>"}, {e.deny, "ask 600"}} {
		r := run(t, c.cmd, c.args)
		contains(t, r, "Usage:")
		if strings.Contains(r.Content, "channel") {
			t.Errorf("%s %q: usage still mentions channels: %s", c.cmd.Name(), c.args, r.Content)
		}
	}
}

func TestAllowDenyGuild(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	ctx := context.Background()

	contains(t, run(t, e.allow, "guild"), "now allowed")
	contains(t, run(t, e.allow, "guild"), "already allowed")
	if ok, _ := e.policy.GuildAllowed(ctx, guildID); !ok {
		t.Fatal("guild not allowed after allow guild")
	}
	contains(t, run(t, e.deny, "guild"), "no longer allowed")
	contains(t, run(t, e.deny, "guild"), "wasn't allowed")
	if ok, _ := e.policy.GuildAllowed(ctx, guildID); ok {
		t.Fatal("guild still allowed after deny guild")
	}
}

func TestAllowDenyGuildByID(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	ctx := context.Background()
	const remote snowflake.ID = 300

	contains(t, run(t, e.allow, "guild 300"), "Server `300` is now allowed.")
	contains(t, run(t, e.allow, "guild 300"), "Server `300` was already allowed.")
	if ok, _ := e.policy.GuildAllowed(ctx, remote); !ok {
		t.Fatal("remote guild not allowed")
	}
	if ok, _ := e.policy.GuildAllowed(ctx, guildID); ok {
		t.Fatal("allowing a remote guild must not allow the current one")
	}
	// Naming the current guild by ID reads the same as the short form.
	contains(t, run(t, e.allow, "guild 100"), "This server is now allowed.")

	contains(t, run(t, e.deny, "guild 300"), "Server `300` is no longer allowed.")
	contains(t, run(t, e.deny, "guild 300"), "Server `300` wasn't allowed.")
	if ok, _ := e.policy.GuildAllowed(ctx, guildID); !ok {
		t.Fatal("denying a remote guild must not deny the current one")
	}
}

func TestAllowDenyUser(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	ctx := context.Background()

	r := run(t, e.allow, "<@2>")
	contains(t, r, "<@2> can now use `play`")
	contains(t, r, "isn't allowed yet") // guild hint
	if e.policy.Allowed(ctx, friendID, guildID, "play") {
		t.Error("grant alone must not allow in an unlisted guild")
	}
	run(t, e.allow, "guild")
	if !e.policy.Allowed(ctx, friendID, guildID, "play") {
		t.Error("friend should be allowed after guild allow + grant")
	}

	contains(t, run(t, e.allow, "<@!2> PLAY"), "already had `play`")
	r = run(t, e.allow, "<@2> play")
	contains(t, r, "already had")
	if strings.Contains(r.Content, "isn't allowed yet") {
		t.Error("no guild hint once the guild is allowed")
	}

	contains(t, run(t, e.deny, "<@2>"), "can no longer use `play`")
	contains(t, run(t, e.deny, "<@2>"), "didn't have `play`")
	if e.policy.Allowed(ctx, friendID, guildID, "play") {
		t.Error("friend still allowed after deny")
	}
}

func TestAllowRefusals(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	contains(t, run(t, e.allow, ""), "Usage:")
	contains(t, run(t, e.allow, "everyone a b"), "Usage:")
	contains(t, run(t, e.deny, "<@2> a b"), "Usage:")
	contains(t, run(t, e.deny, "public a b"), "Usage:")
	contains(t, run(t, e.allow, "<@1>"), "is an owner")
	contains(t, run(t, e.deny, "<@1>"), "config.yaml")
	contains(t, run(t, e.allow, "<@2> dance"), "no `dance` command")
	contains(t, run(t, e.allow, "<@2> allow"), "owner-only and can't be granted")
	contains(t, run(t, e.allow, "<@2> ping"), "always open to everyone")
	contains(t, run(t, e.allow, "everyone dance"), "no `dance` command")
	contains(t, run(t, e.allow, "public allow"), "owner-only and can't be opened")
	contains(t, run(t, e.allow, "everyone ping"), "always open to everyone")

	snap, _ := e.policy.Snapshot(context.Background())
	if len(snap.Grants) != 0 || len(snap.Open) != 0 {
		t.Errorf("refusals must not store anything: %+v", snap)
	}
}

func TestAllowDenyEveryoneAndPublic(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	ctx := context.Background()
	const stranger snowflake.ID = 9

	r := run(t, e.allow, "everyone")
	contains(t, r, "`play` is now open to everyone in this server.")
	contains(t, r, "isn't allowed yet") // guild hint
	if e.policy.Allowed(ctx, stranger, guildID, "play") {
		t.Error("everyone must still need an allowed guild")
	}
	run(t, e.allow, "guild")
	if !e.policy.Allowed(ctx, stranger, guildID, "play") {
		t.Error("everyone in an allowed guild should be allowed")
	}
	r = run(t, e.allow, "EVERYONE PLAY")
	contains(t, r, "`play` was already open to everyone in this server.")
	if strings.Contains(r.Content, "isn't allowed yet") {
		t.Error("no guild hint once the guild is allowed")
	}

	contains(t, run(t, e.allow, "everyone ping"), "always open to everyone in allowed servers. Use `allow public ping` for every server.")
	contains(t, run(t, e.allow, "public ping"), "`ping` is now open to everyone, in every server I'm in.")
	run(t, e.deny, "public ping")
	r = run(t, e.allow, "public play")
	if r.Content != "`play` is now open to everyone, in every server I'm in.\n"+
		"Anyone who can add me to a server can use it there. To keep it to your own servers, "+
		"turn off **Public Bot** in the Developer Portal (your app → Bot), so only you can add me." {
		t.Errorf("allow public: %q (a warning, and no guild hint: public ignores the guild list)", r.Content)
	}
	if strings.Contains(r.Content, "API key") {
		t.Error("the API-key note is only for ask")
	}
	if !e.policy.Allowed(ctx, stranger, 300, "play") {
		t.Error("public should work in an unlisted guild")
	}
	r = run(t, e.allow, "public play")
	if r.Content != "`play` was already open to everyone, in every server I'm in." {
		t.Errorf("already public: %q, want no warning", r.Content)
	}
	if r := run(t, e.allow, "everyone play"); strings.Contains(r.Content, "Public Bot") {
		t.Errorf("allow everyone must not warn about Public Bot: %q", r.Content)
	}

	contains(t, run(t, e.deny, "public"), "`play` is no longer open to everyone, in every server I'm in.")
	contains(t, run(t, e.deny, "public"), "`play` wasn't open to everyone, in every server I'm in.")
	if e.policy.Allowed(ctx, stranger, 300, "play") || !e.policy.Allowed(ctx, stranger, guildID, "play") {
		t.Error("deny public must only remove the public opening")
	}
	contains(t, run(t, e.deny, "everyone play"), "`play` is no longer open to everyone in this server.")
	contains(t, run(t, e.deny, "everyone"), "`play` wasn't open to everyone in this server.")
	if e.policy.Allowed(ctx, stranger, guildID, "play") {
		t.Error("stranger still allowed after deny everyone")
	}
	// Closing takes any name, so stale entries can be cleaned up.
	contains(t, run(t, e.deny, "everyone dance"), "`dance` wasn't open")
}

func TestPublicAskWarnsAboutCost(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	allow := Allow{Access: e.policy, Known: func(string) bool { return true }}
	r := run(t, allow, "public ask")
	contains(t, r, "`ask` is now open to everyone, in every server I'm in.\n"+
		"Anyone who can add me to a server can use it there, each with their own daily allowance, all on your API key. "+
		"To keep it to your own servers, turn off **Public Bot**")
}

func TestDenyGuildCountsKeptOpenings(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	ctx := context.Background()
	run(t, e.allow, "guild")
	for _, c := range []string{"remindme", "play", "ask"} {
		e.policy.Open(ctx, guildID, c, ownerID)
	}
	e.policy.Open(ctx, 300, "skip", ownerID)           // another server's
	e.policy.Open(ctx, access.Public, "play", ownerID) // public
	r := run(t, e.deny, "guild")
	if want := "This server is no longer allowed. Only owners and public commands work there now.\n" +
		"Its 3 open commands (`ask`, `play`, `remindme`) come back if you allow it again."; r.Content != want {
		t.Errorf("deny guild:\n%q\nwant\n%q", r.Content, want)
	}
	// A remote server by ID; its one opening.
	e.policy.AllowGuild(ctx, 300, ownerID)
	contains(t, run(t, e.deny, "guild 300"), "Server `300` is no longer allowed. Only owners and public commands work there now.\n"+
		"Its open command (`skip`) comes back if you allow it again.")
}

// saveFailingBackend fails every write, like a full disk or a down database.
type saveFailingBackend struct{ *accesstest.Memory }

var errSave = errors.New("disk full")

func (saveFailingBackend) AllowGuild(context.Context, snowflake.ID, access.Entry) (bool, error) {
	return false, errSave
}
func (saveFailingBackend) Grant(context.Context, snowflake.ID, string, access.Entry) (bool, error) {
	return false, errSave
}
func (saveFailingBackend) OpenCommand(context.Context, snowflake.ID, string, access.Entry) (bool, error) {
	return false, errSave
}
func (saveFailingBackend) CloseCommand(context.Context, snowflake.ID, string) (bool, error) {
	return false, errSave
}

func TestSaveFailureReply(t *testing.T) {
	e := newEnv(saveFailingBackend{accesstest.NewMemory()})
	for _, c := range []struct {
		cmd  Command
		args string
	}{{e.allow, "guild"}, {e.allow, "<@2>"}, {e.allow, "everyone"}, {e.allow, "public play"}, {e.deny, "everyone"}, {e.deny, "public"}} {
		contains(t, run(t, c.cmd, c.args), "Couldn't save")
	}
}

func TestAccessList(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	r := run(t, e.list, "access")
	contains(t, r, "none. Use `allow guild`")
	contains(t, r, "**Open to everyone** (0)\nnone. Use `allow everyone [command]` in a server, or `allow public [command]` for every server.")

	run(t, e.allow, "guild")
	run(t, e.allow, "<@2>")
	run(t, e.allow, "everyone")
	run(t, e.allow, "public ping")
	run(t, e.allow, "public play")
	e.policy.AllowGuild(context.Background(), 200, ownerID)
	e.policy.Open(context.Background(), 200, "allow", ownerID) // refused: owner-only
	e.policy.Open(context.Background(), 200, "ping", ownerID)  // refused: always open
	e.policy.Open(context.Background(), 300, "play", ownerID)  // a server that isn't allowed

	r = run(t, e.list, "access")
	for _, want := range []string{
		"**Allowed servers** (2)",
		"`100` (this server), added by <@1> <t:1790658000:d>",
		"`200`, added by <@1>",
		"**Grants** (1 users)",
		"- <@2>: `play`",
		"**Open to everyone** (4)\n- every server (public): `ping`, `play`\n- `100` (this server): `play`\n- `300` (not allowed: inactive): `play`",
	} {
		contains(t, r, want)
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("é", 2500)
	got := truncate(long, maxMessageLen)
	if n := utf8.RuneCountInString(got); n != maxMessageLen {
		t.Errorf("truncated length = %d, want %d", n, maxMessageLen)
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Error("missing truncation marker")
	}
	if truncate("short", maxMessageLen) != "short" {
		t.Error("short strings must be unchanged")
	}
}

func TestAllowInheritedCommand(t *testing.T) {
	p := access.NewPolicy(accesstest.NewMemory(), access.Options{
		Owners:  []snowflake.ID{ownerID},
		Inherit: map[string]string{"skip": "play"},
	})
	a := Allow{Access: p, Known: func(string) bool { return true }}
	contains(t, run(t, a, "<@2> skip"), "`skip` comes with `play` access. Grant `play` instead.")
}

// runIn is run from a given channel.
func runIn(t *testing.T, cmd Command, channel snowflake.ID, args string) Reply {
	t.Helper()
	rep := &fakeReplier{}
	if err := cmd.Run(context.Background(), Request{GuildID: guildID, ChannelID: channel, AuthorID: ownerID, Args: args, Reply: rep}); err != nil {
		t.Fatalf("Run(%q): %v", args, err)
	}
	if len(rep.got) != 1 || len(rep.got[0].Mentions) != 0 {
		t.Fatalf("Run(%q): replies %+v, want one that pings no one", args, rep.got)
	}
	return rep.got[0]
}
