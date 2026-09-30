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
		Owners:    []snowflake.ID{owner},
		Public:    []string{"ping"},
		OwnerOnly: []string{"allow", "deny", "access"},
		Now:       func() time.Time { return fixedNow },
		Logger:    logger,
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
	if _, err := p.Grant(ctx, friend, "ping", owner); !errors.Is(err, access.ErrPublic) {
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
		Owners:    []snowflake.ID{owner},
		Public:    []string{"ping"},
		OwnerOnly: []string{"allow"},
		Inherit:   map[string]string{"Skip": "play", "stop": "PLAY", "echo": "ping", "sneaky": "allow"},
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
func (failingBackend) ChannelAllowed(context.Context, snowflake.ID, string) (bool, error) {
	return false, errDown
}
