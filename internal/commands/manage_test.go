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
		Owners:    []snowflake.ID{ownerID},
		Public:    []string{"ping"},
		OwnerOnly: ManagementCommands,
		Now:       func() time.Time { return time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC) },
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
		{"ask", target{channel: true, command: "ask"}, true},
		{"ASK <#500>", target{channel: true, channelID: 500, command: "ask"}, true},
		{"ask 1100000000000000005", target{channel: true, channelID: 1100000000000000005, command: "ask"}, true},
		{"ask #general", target{}, false},
		{"ask <#0>", target{}, false},
		{"ask <@500>", target{}, false}, // a user, not a channel
		{"ask <#500> extra", target{}, false},
	}
	for _, c := range cases {
		got, ok := parseTarget(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseTarget(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
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
	contains(t, run(t, e.allow, "everyone"), "Usage:")
	contains(t, run(t, e.deny, "<@2> a b"), "Usage:")
	contains(t, run(t, e.allow, "<@1>"), "is an owner")
	contains(t, run(t, e.deny, "<@1>"), "config.yaml")
	contains(t, run(t, e.allow, "<@2> dance"), "no `dance` command")
	contains(t, run(t, e.allow, "<@2> allow"), "owner-only")
	contains(t, run(t, e.allow, "<@2> ping"), "already open to everyone")

	snap, _ := e.policy.Snapshot(context.Background())
	if len(snap.Grants) != 0 {
		t.Errorf("refusals must not store grants: %+v", snap.Grants)
	}
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

func TestSaveFailureReply(t *testing.T) {
	e := newEnv(saveFailingBackend{accesstest.NewMemory()})
	contains(t, run(t, e.allow, "guild"), "Couldn't save")
	contains(t, run(t, e.allow, "<@2>"), "Couldn't save")
}

func TestAccessList(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	contains(t, run(t, e.list, "access"), "none. Use `allow guild`")

	run(t, e.allow, "guild")
	run(t, e.allow, "<@2>")
	e.policy.AllowGuild(context.Background(), 200, ownerID)

	r := run(t, e.list, "access")
	for _, want := range []string{
		"**Allowed servers** (2)",
		"`100` (this server), added by <@1> <t:1790658000:d>",
		"`200`, added by <@1>",
		"**Grants** (1 users)",
		"- <@2>: `play`",
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

func TestAllowDenyAskChannel(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	e.allow.ChannelVisible = func(id snowflake.ID) bool { return id == 500 }
	ctx := context.Background()
	askOK := func(ch snowflake.ID) bool {
		snap, err := e.policy.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range snap.Channels {
			if c.ChannelID == ch && c.Command == "ask" {
				return true
			}
		}
		return false
	}

	// No channel given: the current one. A visible channel gets no note.
	r := runIn(t, e.allow, 500, "ask")
	if r.Content != "`ask` is now enabled in <#500>." {
		t.Errorf("reply %q", r.Content)
	}
	contains(t, runIn(t, e.allow, 500, "ask <#500>"), "already enabled")
	// By ID, from another channel (another server's channel works the same way).
	contains(t, runIn(t, e.allow, 500, "ask 600"), "`ask` is now enabled in <#600>.")
	contains(t, runIn(t, e.allow, 500, "ask 600"), "already enabled")
	if !askOK(500) || !askOK(600) {
		t.Fatal("channels not saved")
	}

	contains(t, runIn(t, e.deny, 700, "ask 600"), "`ask` is no longer enabled in <#600>.")
	contains(t, runIn(t, e.deny, 700, "ask 600"), "wasn't enabled")
	contains(t, runIn(t, e.deny, 500, "ask"), "no longer enabled in <#500>")
	if askOK(500) || askOK(600) {
		t.Error("channels still saved after deny")
	}
}

func TestAllowAskUnseenChannelIsSavedWithANote(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	e.allow.ChannelVisible = func(snowflake.ID) bool { return false }
	r := runIn(t, e.allow, 500, "ask 900")
	contains(t, r, "now enabled in <#900>")
	contains(t, r, "can't see channel `900`")
	snap, _ := e.policy.Snapshot(context.Background())
	if len(snap.Channels) != 1 || snap.Channels[0].ChannelID != 900 {
		t.Errorf("channels = %+v", snap.Channels)
	}
}

func TestAllowAskBadChannelShowsUsage(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	contains(t, runIn(t, e.allow, 500, "ask #general"), "allow ask [#channel or channel ID]")
	contains(t, runIn(t, e.deny, 500, "ask nope"), "deny ask [#channel or channel ID]")
}

func TestAccessListsAskChannels(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	if strings.Contains(run(t, e.list, "access").Content, "Channels") {
		t.Error("no channels: the section should be hidden")
	}
	runIn(t, e.allow, 500, "ask")
	contains(t, run(t, e.list, "access"), "**Channels** (1)\n- `ask` in <#500> (`500`), added by <@1>")
}
