package access_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
)

// Policy tests use the in-memory backend. They depend only on the Backend
// interface, so they stay unchanged whatever storage production uses.

const (
	owner  snowflake.ID = 1
	friend snowflake.ID = 2
	other  snowflake.ID = 3
	guildA snowflake.ID = 100
	guildB snowflake.ID = 200
)

var fixedNow = time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)

func newPolicy(b access.Backend, logger *slog.Logger) *access.Policy {
	return access.NewPolicy(b, access.Options{
		Owners:     []snowflake.ID{owner},
		AlwaysOpen: []string{"ping"},
		OwnerOnly:  []string{"allow", "deny", "access"},
		Inherit:    map[string]string{"skip": "play"},
		Now:        func() time.Time { return fixedNow },
		Logger:     logger,
	})
}

func TestAllowedRule(t *testing.T) {
	ctx := context.Background()
	p := newPolicy(accesstest.NewMemory(), nil)
	if _, err := p.AllowGuild(ctx, guildA, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant(ctx, friend, "Play", owner); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		user    snowflake.ID
		guild   snowflake.ID
		command string
		want    bool
	}{
		{"owner ping in allowed guild", owner, guildA, "ping", true},
		{"owner ping in unlisted guild", owner, guildB, "ping", true},
		{"granted friend ping in unlisted guild", friend, guildB, "ping", false},
		{"owner in allowed guild", owner, guildA, "play", true},
		{"owner in unlisted guild", owner, guildB, "play", true},
		{"owner owner-only command", owner, guildB, "allow", true},
		{"owner unknown command", owner, guildB, "nosuch", true},
		{"friend granted, allowed guild", friend, guildA, "play", true},
		{"friend granted, case-insensitive", friend, guildA, "PLAY", true},
		{"friend granted, unlisted guild", friend, guildB, "play", false},
		{"friend not granted command", friend, guildA, "image", false},
		{"public in allowed guild", other, guildA, "ping", true},
		{"public in unlisted guild", other, guildB, "ping", false},
		{"ungranted user", other, guildA, "play", false},
		{"unknown command", other, guildA, "nosuch", false},
		{"owner-only for non-owner", friend, guildA, "allow", false},
	}
	for _, c := range cases {
		if got := p.Allowed(ctx, c.user, c.guild, c.command); got != c.want {
			t.Errorf("%s: Allowed(%d, %d, %q) = %v, want %v", c.name, c.user, c.guild, c.command, got, c.want)
		}
	}
}

func TestGrantRefusals(t *testing.T) {
	ctx := context.Background()
	p := newPolicy(accesstest.NewMemory(), nil)
	if _, err := p.Grant(ctx, friend, "Allow", owner); !errors.Is(err, access.ErrOwnerOnly) {
		t.Errorf("grant owner-only: err = %v", err)
	}
	if _, err := p.Grant(ctx, friend, "ping", owner); !errors.Is(err, access.ErrAlwaysOpen) {
		t.Errorf("grant public: err = %v", err)
	}
	snap, _ := p.Snapshot(ctx)
	if len(snap.Grants) != 0 {
		t.Errorf("refused grants must not be stored: %+v", snap.Grants)
	}
}

func TestEntriesRecordWhoAndWhen(t *testing.T) {
	ctx := context.Background()
	p := newPolicy(accesstest.NewMemory(), nil)
	p.AllowGuild(ctx, guildA, owner)
	p.Grant(ctx, friend, "PLAY", owner)
	snap, _ := p.Snapshot(ctx)
	g, gr := snap.Guilds[0], snap.Grants[0]
	if g.AddedBy != owner || !g.AddedAt.Equal(fixedNow) {
		t.Errorf("guild entry = %+v", g)
	}
	if gr.Command != "play" || gr.AddedBy != owner || !gr.AddedAt.Equal(fixedNow) {
		t.Errorf("grant entry = %+v (command must be stored lowercase)", gr)
	}
}

func TestChangesAreLogged(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	p := newPolicy(accesstest.NewMemory(), slog.New(slog.NewTextHandler(&buf, nil)))
	p.AllowGuild(ctx, guildA, owner)
	p.AllowGuild(ctx, guildA, owner) // no-op: not logged
	p.Grant(ctx, friend, "play", owner)
	p.Revoke(ctx, friend, "play", owner)
	p.DenyGuild(ctx, guildA, owner)

	out := buf.String()
	for _, want := range []string{"action=\"allow guild\"", "action=grant", "action=revoke", "action=\"deny guild\""} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "allow guild"); n != 1 {
		t.Errorf("no-op change logged: %d allow lines", n)
	}
}

// failingBackend returns errors, to check that Policy fails closed.
type failingBackend struct{ *accesstest.Memory }

var errDown = errors.New("storage down")

func (failingBackend) GuildAllowed(context.Context, snowflake.ID) (bool, error) {
	return false, errDown
}
func (failingBackend) HasGrant(context.Context, snowflake.ID, string) (bool, error) {
	return false, errDown
}
func (failingBackend) AllowGuild(context.Context, snowflake.ID, access.Entry) (bool, error) {
	return false, errDown
}

func (failingBackend) CommandOpen(context.Context, snowflake.ID, string) (bool, error) {
	return false, errDown
}

func TestBackendErrorsFailClosed(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	p := newPolicy(failingBackend{accesstest.NewMemory()}, slog.New(slog.NewTextHandler(&buf, nil)))
	if p.Allowed(ctx, other, guildA, "ping") {
		t.Error("backend error must deny")
	}
	if !p.Allowed(ctx, owner, guildA, "ping") {
		t.Error("owners must not depend on the backend")
	}
	if _, err := p.AllowGuild(ctx, guildA, owner); !errors.Is(err, errDown) {
		t.Errorf("AllowGuild err = %v", err)
	}
	if !strings.Contains(buf.String(), "storage down") {
		t.Errorf("backend errors should be logged:\n%s", buf.String())
	}
	// Check hands the error back instead (for callers that delete on "no").
	if ok, err := p.Check(ctx, other, guildA, "play"); ok || !errors.Is(err, errDown) {
		t.Errorf("Check = %v, %v; want false and the error", ok, err)
	}
	if ok, err := p.Check(ctx, owner, guildA, "play"); !ok || err != nil {
		t.Errorf("Check for an owner = %v, %v", ok, err)
	}
}

// Each step of the check can fail on its own; none may fall through to "yes".
func TestEachBackendErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, failing := range []string{"public", "guild", "everyone", "grant"} {
		mem := accesstest.NewMemory()
		p := newPolicy(stepFailing{mem, failing}, nil)
		p.AllowGuild(ctx, guildA, owner)
		p.Grant(ctx, friend, "play", owner)
		ok, err := p.Check(ctx, friend, guildA, "play")
		if ok || !errors.Is(err, errDown) {
			t.Errorf("%s failing: Check = %v, %v; want false and the error", failing, ok, err)
		}
	}
}

// stepFailing fails one step of the access check: the public lookup, the
// guild allow-list, the guild's open commands, or the grants.
type stepFailing struct {
	*accesstest.Memory
	step string
}

func (s stepFailing) CommandOpen(ctx context.Context, guildID snowflake.ID, command string) (bool, error) {
	if (s.step == "public" && guildID == access.Public) || (s.step == "everyone" && guildID != access.Public) {
		return false, errDown
	}
	return s.Memory.CommandOpen(ctx, guildID, command)
}

func (s stepFailing) GuildAllowed(ctx context.Context, guildID snowflake.ID) (bool, error) {
	if s.step == "guild" {
		return false, errDown
	}
	return s.Memory.GuildAllowed(ctx, guildID)
}

func (s stepFailing) HasGrant(ctx context.Context, userID snowflake.ID, command string) (bool, error) {
	if s.step == "grant" {
		return false, errDown
	}
	return s.Memory.HasGrant(ctx, userID, command)
}

// The levels, widest first: owner, public (any guild, allowed or not),
// everyone in an allowed guild, then a personal grant in an allowed guild.
func TestOpenCommandLevels(t *testing.T) {
	ctx := context.Background()
	const guildC snowflake.ID = 300 // allowed, nothing opened
	p := newPolicy(accesstest.NewMemory(), nil)
	p.AllowGuild(ctx, guildA, owner)
	p.AllowGuild(ctx, guildC, owner)
	mustChange(t, "open play in A", true)(p.Open(ctx, guildA, "play", owner))
	mustChange(t, "open remindme publicly", true)(p.Open(ctx, access.Public, "REMINDME", owner))
	mustChange(t, "open ask in B (not allowed)", true)(p.Open(ctx, guildB, "ask", owner))
	p.Grant(ctx, friend, "ask", owner)

	type check struct {
		user    snowflake.ID
		guild   snowflake.ID
		command string
		want    bool
	}
	run := func(stage string, cases []check) {
		t.Helper()
		for _, c := range cases {
			if got := p.Allowed(ctx, c.user, c.guild, c.command); got != c.want {
				t.Errorf("%s: Allowed(user %d, guild %d, %q) = %v, want %v", stage, c.user, c.guild, c.command, got, c.want)
			}
		}
	}
	run("opened", []check{
		{other, guildA, "play", true},  // everyone in A
		{other, guildA, "PLAY", true},  // case-insensitive
		{other, guildA, "skip", true},  // inherits play
		{other, guildC, "play", false}, // allowed, but not opened there
		{other, guildB, "play", false}, // not allowed
		{other, guildA, "remindme", true},
		{other, guildB, "remindme", true}, // public ignores the guild list
		{other, guildC, "remindme", true},
		{other, guildB, "ask", false}, // everyone in B, but B isn't allowed
		{friend, guildB, "ask", false},
		{friend, guildA, "ask", true}, // the grant
		{other, guildA, "ask", false},
		{other, guildA, "allow", false}, // owner-only stays owner-only
		{other, guildB, "ping", false},  // always-open still needs an allowed guild
		{other, guildA, "ping", true},
	})

	p.DenyGuild(ctx, guildA, owner)
	run("guild A denied", []check{
		{other, guildA, "play", false},
		{other, guildA, "skip", false},
		{other, guildA, "remindme", true}, // public
		{friend, guildA, "ask", false},
	})
	p.AllowGuild(ctx, guildA, owner)
	run("guild A allowed again", []check{{other, guildA, "play", true}}) // its opening was kept

	p.AllowGuild(ctx, guildB, owner)
	run("guild B allowed", []check{{other, guildB, "ask", true}}) // opened before it was allowed

	p.Grant(ctx, friend, "play", owner)
	mustChange(t, "close play in A", true)(p.Close(ctx, guildA, "Play", owner))
	mustChange(t, "close it again", false)(p.Close(ctx, guildA, "play", owner))
	mustChange(t, "close remindme publicly", true)(p.Close(ctx, access.Public, "remindme", owner))
	run("closed", []check{
		{other, guildA, "play", false},
		{friend, guildA, "play", true}, // their own grant
		{friend, guildA, "skip", true},
		{other, guildA, "remindme", false},
		{other, guildB, "remindme", false},
		{owner, guildB, "remindme", true},
		{owner, guildA, "allow", true},
	})
	// A grant can't block a wider level: revoking friend's ask leaves B's opening.
	p.Revoke(ctx, friend, "ask", owner)
	run("revoked", []check{{friend, guildB, "ask", true}, {friend, guildA, "ask", false}})
}

func TestOpenRefusalsAndEntries(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	p := newPolicy(accesstest.NewMemory(), slog.New(slog.NewTextHandler(&buf, nil)))
	for _, guild := range []snowflake.ID{guildA, access.Public} {
		for cmd, want := range map[string]error{"Allow": access.ErrOwnerOnly, "SKIP": access.ErrInherited} {
			if _, err := p.Open(ctx, guild, cmd, owner); !errors.Is(err, want) {
				t.Errorf("open %q in %d: err = %v, want %v", cmd, guild, err, want)
			}
		}
	}
	// An always-open command can't be opened in one guild (it already is),
	// nor granted, but it can be made public.
	if _, err := p.Open(ctx, guildA, "ping", owner); !errors.Is(err, access.ErrAlwaysOpen) {
		t.Errorf("open ping in a guild: err = %v, want ErrAlwaysOpen", err)
	}
	if _, err := p.Grant(ctx, friend, "ping", owner); !errors.Is(err, access.ErrAlwaysOpen) {
		t.Errorf("grant ping: err = %v, want ErrAlwaysOpen", err)
	}
	if p.Allowed(ctx, other, guildB, "ping") {
		t.Fatal("setup: ping in an unlisted guild")
	}
	mustChange(t, "open ping publicly", true)(p.Open(ctx, access.Public, "PING", owner))
	if !p.Allowed(ctx, other, guildB, "ping") {
		t.Error("public ping must work in an unlisted guild")
	}
	mustChange(t, "close public ping", true)(p.Close(ctx, access.Public, "ping", owner))
	if p.Allowed(ctx, other, guildB, "ping") {
		t.Error("ping still works in an unlisted guild after closing")
	}
	mustChange(t, "open", true)(p.Open(ctx, guildA, "Play", owner))
	mustChange(t, "close a stale name", false)(p.Close(ctx, guildA, "nosuch", owner))
	snap, _ := p.Snapshot(ctx)
	if len(snap.Open) != 1 {
		t.Fatalf("refused openings must not be stored: %+v", snap.Open)
	}
	if o := snap.Open[0]; o.GuildID != guildA || o.Command != "play" || o.AddedBy != owner || !o.AddedAt.Equal(fixedNow) {
		t.Errorf("open entry = %+v (command must be stored lowercase)", o)
	}
	p.Close(ctx, guildA, "play", owner)
	for _, want := range []string{"action=open", "action=close", "command=play"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %s:\n%s", want, buf.String())
		}
	}
}

func mustChange(t *testing.T, what string, want bool) func(bool, error) {
	t.Helper()
	return func(changed bool, err error) {
		t.Helper()
		if err != nil || changed != want {
			t.Errorf("%s: changed = %v, err = %v; want %v", what, changed, err, want)
		}
	}
}

func TestGuildDenyRevokesPublicAccess(t *testing.T) {
	ctx := context.Background()
	p := newPolicy(accesstest.NewMemory(), nil)
	p.AllowGuild(ctx, guildA, owner)
	p.Grant(ctx, friend, "play", owner)
	if !p.Allowed(ctx, other, guildA, "ping") || !p.Allowed(ctx, friend, guildA, "play") {
		t.Fatal("setup: expected access in allowed guild")
	}
	p.DenyGuild(ctx, guildA, owner)
	if p.Allowed(ctx, other, guildA, "ping") {
		t.Error("ping still allowed for non-owner after deny guild")
	}
	if p.Allowed(ctx, friend, guildA, "play") {
		t.Error("grant still usable after deny guild")
	}
	if !p.Allowed(ctx, owner, guildA, "ping") {
		t.Error("owner must keep access after deny guild")
	}
	// Re-allowing restores the friend's grant, which deny guild kept.
	p.AllowGuild(ctx, guildA, owner)
	if !p.Allowed(ctx, friend, guildA, "play") {
		t.Error("grant should work again after re-allowing the guild")
	}
}

func TestInheritedCommandsShareParentAccess(t *testing.T) {
	ctx := context.Background()
	p := access.NewPolicy(accesstest.NewMemory(), access.Options{
		Owners:     []snowflake.ID{owner},
		AlwaysOpen: []string{"ping"},
		OwnerOnly:  []string{"allow"},
		Inherit:    map[string]string{"Skip": "play", "stop": "PLAY", "echo": "ping", "sneaky": "allow"},
	})
	p.AllowGuild(ctx, guildA, owner)
	p.Grant(ctx, friend, "play", owner)

	cases := []struct {
		name    string
		user    snowflake.ID
		guild   snowflake.ID
		command string
		want    bool
	}{
		{"granted parent, child allowed", friend, guildA, "skip", true},
		{"child case-insensitive", friend, guildA, "STOP", true},
		{"no parent grant, child denied", other, guildA, "skip", false},
		{"child still needs allowed guild", friend, guildB, "skip", false},
		{"child of public is public", other, guildA, "echo", true},
		{"child of owner-only stays owner-only", friend, guildA, "sneaky", false},
		{"owner always", owner, guildB, "skip", true},
	}
	for _, c := range cases {
		if got := p.Allowed(ctx, c.user, c.guild, c.command); got != c.want {
			t.Errorf("%s: Allowed(%d, %d, %q) = %v, want %v", c.name, c.user, c.guild, c.command, got, c.want)
		}
	}

	if _, err := p.Grant(ctx, other, "skip", owner); !errors.Is(err, access.ErrInherited) {
		t.Errorf("granting an inheriting command: err = %v, want ErrInherited", err)
	}
	if parent, ok := p.InheritsFrom("SKIP"); !ok || parent != "play" {
		t.Errorf("InheritsFrom(SKIP) = %q, %v", parent, ok)
	}
	// Revoking the parent removes the child too.
	p.Revoke(ctx, friend, "play", owner)
	if p.Allowed(ctx, friend, guildA, "skip") {
		t.Error("child still allowed after parent revoked")
	}
}
