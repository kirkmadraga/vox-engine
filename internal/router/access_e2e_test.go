package router

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
	"github.com/kirkmadraga/vox-engine/internal/commands"
)

// These tests send real message text through the router, the real access Policy
// (over the in-memory backend) and the real commands, and check whether the
// bot replies. They catch wiring mistakes the unit tests with fakes cannot.

const (
	e2eOwner    snowflake.ID = 7
	e2eFriend   snowflake.ID = 8
	e2eStranger snowflake.ID = 9
	allowedG    snowflake.ID = 100
	unlistedG   snowflake.ID = 200
)

type e2e struct {
	t      *testing.T
	router *Router
}

func newE2E(t *testing.T) e2e {
	t.Helper()
	policy := access.NewPolicy(accesstest.NewMemory(), access.Options{
		Owners:    []snowflake.ID{e2eOwner},
		Public:    []string{"ping"},
		OwnerOnly: commands.ManagementCommands,
	})
	var reg *commands.Registry
	known := func(name string) bool { _, ok := reg.Lookup(name); return ok }
	reg, err := commands.NewRegistry(
		commands.Ping{},
		commands.Allow{Access: policy, Known: known},
		commands.Deny{Access: policy},
		commands.AccessList{Access: policy},
		named("play"), // stand-in so "play" can be granted before the real command exists
	)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return e2e{t: t, router: New(func() snowflake.ID { return self }, reg, policy, logger)}
}

// send delivers "<@bot> text" from user in guild and returns the replies.
func (e e2e) send(user, guild snowflake.ID, text string) []commands.Reply {
	e.t.Helper()
	rep := &fakeReplier{}
	e.router.Handle(context.Background(), Message{GuildID: guild, ChannelID: 1, AuthorID: user, Content: "<@1000> " + text}, rep)
	return rep.got
}

func (e e2e) wantReply(user, guild snowflake.ID, text, want string) {
	e.t.Helper()
	got := e.send(user, guild, text)
	if len(got) != 1 {
		e.t.Fatalf("user %d in guild %d sent %q: got %d replies, want 1 (%q)", user, guild, text, len(got), want)
	}
	if got[0].Content != want {
		e.t.Errorf("user %d in guild %d sent %q: reply %q, want %q", user, guild, text, got[0].Content, want)
	}
}

func (e e2e) wantSilence(user, guild snowflake.ID, text string) {
	e.t.Helper()
	if got := e.send(user, guild, text); len(got) != 0 {
		e.t.Errorf("user %d in guild %d sent %q: want no reply, got %+v", user, guild, text, got)
	}
}

func TestE2EPingByGuildAndRole(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")

	// Owner: answered everywhere.
	e.wantReply(e2eOwner, allowedG, "ping", "<@7> pong")
	e.wantReply(e2eOwner, unlistedG, "ping", "<@7> pong")

	// Non-owner: answered only in allowed guilds, silent elsewhere.
	e.wantReply(e2eStranger, allowedG, "ping", "<@9> pong")
	e.wantSilence(e2eStranger, unlistedG, "ping")
}

func TestE2EUnknownCommand(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")

	e.wantReply(e2eOwner, unlistedG, "dance", "unknown command")
	e.wantSilence(e2eStranger, allowedG, "dance")
	e.wantSilence(e2eStranger, unlistedG, "dance")
}

func TestE2EManagementIsOwnerOnly(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")

	for _, cmd := range []string{"allow guild", "deny guild", "allow <@9>", "deny <@8>", "access"} {
		e.wantSilence(e2eStranger, allowedG, cmd)
		e.wantSilence(e2eStranger, unlistedG, cmd)
	}
	// A non-owner's attempt must not have changed anything.
	e.wantReply(e2eStranger, allowedG, "ping", "<@9> pong")
	e.wantSilence(e2eStranger, unlistedG, "ping")
}

func TestE2EGrantNeedsAllowedGuild(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.send(e2eOwner, allowedG, "allow <@8> play")

	e.wantReply(e2eFriend, allowedG, "play", "") // stand-in command replies ""
	e.wantSilence(e2eFriend, unlistedG, "play")  // grant alone is not enough
	e.wantSilence(e2eStranger, allowedG, "play") // no grant
}

func TestE2EDenyGuildSilencesNonOwners(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.send(e2eOwner, allowedG, "allow <@8> play")
	e.wantReply(e2eStranger, allowedG, "ping", "<@9> pong")

	e.wantReply(e2eOwner, allowedG, "deny guild", "This server is no longer allowed. I'll only respond to owners there.")

	e.wantSilence(e2eStranger, allowedG, "ping")
	e.wantSilence(e2eFriend, allowedG, "play")
	e.wantReply(e2eOwner, allowedG, "ping", "<@7> pong")
}

func TestE2ERemoteGuildAllow(t *testing.T) {
	e := newE2E(t)
	// The owner allows the other guild by ID from here, without being there.
	e.wantReply(e2eOwner, allowedG, "allow guild 200", "Server `200` is now allowed.")
	e.wantReply(e2eStranger, unlistedG, "ping", "<@9> pong")
	e.wantSilence(e2eStranger, allowedG, "ping") // the current guild was not allowed
}

// named is a stand-in command that replies with an empty message.
type named string

func (n named) Name() string { return string(n) }
func (named) Run(ctx context.Context, req commands.Request) error {
	return req.Reply.Reply(ctx, commands.Reply{})
}
