package router

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

// newE2E builds the router like main does (fallback "ask"); extra commands are
// registered too, e.g. the ask command.
func newE2E(t *testing.T, extra ...commands.Command) e2e {
	t.Helper()
	policy := access.NewPolicy(accesstest.NewMemory(), access.Options{
		Owners:     []snowflake.ID{e2eOwner},
		AlwaysOpen: []string{"ping"},
		OwnerOnly:  commands.ManagementCommands,
		Inherit:    map[string]string{commands.ForgetCommand: commands.AskCommand, "skip": "play"},
	})
	var reg *commands.Registry
	known := func(name string) bool { _, ok := reg.Lookup(name); return ok }
	reg, err := commands.NewRegistry(append([]commands.Command{
		commands.Ping{},
		commands.Allow{Access: policy, Known: known},
		commands.Deny{Access: policy},
		commands.Debug{Access: policy},
		named("play"), // stand-in so "play" can be granted before the real command exists
		named("skip"),
	}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(func() snowflake.ID { return self }, reg, policy, logger)
	r.Fallback = commands.AskCommand
	return e2e{t: t, router: r}
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

	for _, cmd := range []string{"allow guild", "deny guild", "allow <@9>", "deny <@8>", "debug", "debug access", "debug llm"} {
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

	e.wantReply(e2eOwner, allowedG, "deny guild", "This server is no longer allowed. Only owners and public commands work there now.")

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

// "allow everyone": anyone in that allowed server, no grants needed.
func TestE2EAllowEveryone(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.wantSilence(e2eStranger, allowedG, "play")

	e.wantReply(e2eOwner, allowedG, "allow everyone", "`play` is now open to everyone in this server.") // play by default
	e.wantReply(e2eOwner, allowedG, "allow everyone play", "`play` was already open to everyone in this server.")
	e.wantReply(e2eStranger, allowedG, "play", "")
	e.wantReply(e2eStranger, allowedG, "skip", "") // comes with play
	e.wantSilence(e2eStranger, unlistedG, "play")  // only that server
	e.wantSilence(e2eStranger, allowedG, "ask hi") // only that command

	// Opening in a server that isn't allowed is stored, with a warning, and
	// works once it's allowed.
	e.wantReply(e2eOwner, unlistedG, "allow everyone", "`play` is now open to everyone in this server.\nNote: this server isn't allowed yet. Use `allow guild` to enable it here.")
	e.wantSilence(e2eStranger, unlistedG, "play")
	e.send(e2eOwner, unlistedG, "allow guild")
	e.wantReply(e2eStranger, unlistedG, "play", "")

	// A grant can't be taken from a command open to everyone.
	e.send(e2eOwner, allowedG, "allow <@8> play")
	e.send(e2eOwner, allowedG, "deny <@8> play")
	e.wantReply(e2eFriend, allowedG, "play", "")

	// Denying the server stops it (only owners and public commands), but keeps the opening.
	e.send(e2eOwner, allowedG, "deny guild")
	e.wantSilence(e2eStranger, allowedG, "play")
	e.send(e2eOwner, allowedG, "allow guild")
	e.wantReply(e2eStranger, allowedG, "play", "")

	e.wantReply(e2eOwner, allowedG, "deny everyone play", "`play` is no longer open to everyone in this server.")
	e.wantReply(e2eOwner, allowedG, "deny everyone", "`play` wasn't open to everyone in this server.")
	e.wantSilence(e2eStranger, allowedG, "play")
	e.wantSilence(e2eStranger, allowedG, "skip")
	e.wantReply(e2eStranger, unlistedG, "play", "") // the other server's opening stays
}

// "allow public": anyone, in any server, allowed or not.
func TestE2EAllowPublic(t *testing.T) {
	e := newE2E(t)
	e.wantSilence(e2eStranger, unlistedG, "play")
	e.wantReply(e2eOwner, unlistedG, "allow public play", "`play` is now open to everyone, in every server I'm in.\n"+
		"Anyone who can add me to a server can use it there. To keep it to your own servers, "+
		"turn off **Public Bot** in the Developer Portal (your app → Bot), so only you can add me.")
	e.wantReply(e2eOwner, allowedG, "allow public PLAY", "`play` was already open to everyone, in every server I'm in.")
	for _, g := range []snowflake.ID{allowedG, unlistedG, 300} {
		e.wantReply(e2eStranger, g, "play", "")
		e.wantReply(e2eStranger, g, "skip", "")
		e.wantSilence(e2eStranger, g, "ping") // ping still needs an allowed server
	}
	e.send(e2eOwner, allowedG, "allow guild")
	e.send(e2eOwner, allowedG, "deny guild")
	e.wantReply(e2eStranger, allowedG, "play", "") // public ignores the guild list

	e.wantReply(e2eOwner, allowedG, "deny public play", "`play` is no longer open to everyone, in every server I'm in.")
	e.wantReply(e2eOwner, allowedG, "deny public play", "`play` wasn't open to everyone, in every server I'm in.")
	e.wantSilence(e2eStranger, unlistedG, "play")
	e.wantSilence(e2eStranger, allowedG, "play")
}

func TestE2EOpenRefusals(t *testing.T) {
	e := newE2E(t)
	for _, scope := range []string{"everyone", "public"} {
		e.wantReply(e2eOwner, allowedG, "allow "+scope+" debug", "`debug` is owner-only and can't be opened.")
		e.wantReply(e2eOwner, allowedG, "allow "+scope+" allow", "`allow` is owner-only and can't be opened.")
		e.wantReply(e2eOwner, allowedG, "allow "+scope+" skip", "`skip` comes with `play` access. Open `play` instead.")
		e.wantReply(e2eOwner, allowedG, "allow "+scope+" dance", "There's no `dance` command (yet).")
		e.wantReply(e2eOwner, allowedG, "deny "+scope+" dance", "`dance` wasn't open to "+map[string]string{"everyone": "everyone in this server", "public": "everyone, in every server I'm in"}[scope]+".")
		// Strangers can't open anything.
		e.wantSilence(e2eStranger, allowedG, "allow "+scope+" play")
		e.wantSilence(e2eStranger, allowedG, "deny "+scope+" play")
	}
	e.wantSilence(e2eStranger, unlistedG, "play") // nothing was opened
	e.wantReply(e2eOwner, allowedG, "allow everyone ping", "`ping` is always open to everyone in allowed servers. Use `allow public ping` for every server.")
	e.wantReply(e2eOwner, allowedG, "allow everyone play extra", "Usage: `allow guild [serverID]` (defaults to this server), `allow @user [command]`, "+
		"`allow everyone [command]` (everyone in this server) or `allow public [command]` (everyone, in every server); the command defaults to `play`.")
	e.wantReply(e2eOwner, allowedG, "allow <@8> ping", "`ping` is always open to everyone in allowed servers.")
}

func TestE2EDebugAccessListsOpenCommands(t *testing.T) {
	e := newE2E(t)
	e.send(e2eOwner, allowedG, "allow guild")
	got := e.send(e2eOwner, allowedG, "debug access")
	if len(got) != 1 || !strings.Contains(got[0].Content, "**Open to everyone** (0)\nnone.") {
		t.Fatalf("debug access, nothing open: %+v", got)
	}
	e.send(e2eOwner, allowedG, "allow everyone play")
	e.send(e2eOwner, unlistedG, "allow everyone play")
	e.send(e2eOwner, allowedG, "allow public skip") // refused: not stored
	e.send(e2eOwner, allowedG, "allow public play")
	got = e.send(e2eOwner, allowedG, "debug access")
	want := "**Open to everyone** (3)\n- every server (public): `play`\n- `100` (this server): `play`\n- `200` (not allowed: inactive): `play`"
	if len(got) != 1 || !strings.HasSuffix(got[0].Content, want) {
		t.Errorf("debug access:\n%s\nwant it to end with:\n%s", got[0].Content, want)
	}
	// Denying this server keeps its opening, marked inactive, and says so.
	e.wantReply(e2eOwner, allowedG, "deny guild", "This server is no longer allowed. Only owners and public commands work there now.\n"+
		"Its open command (`play`) comes back if you allow it again.")
	got = e.send(e2eOwner, allowedG, "debug access")
	if len(got) != 1 || !strings.Contains(got[0].Content, "- `100` (this server, not allowed: inactive): `play`") {
		t.Errorf("debug access after deny guild:\n%s", got[0].Content)
	}
}

// "allow public ping": ping then answers in servers that aren't allowed too.
func TestE2EPublicPing(t *testing.T) {
	e := newE2E(t)
	e.wantSilence(e2eStranger, unlistedG, "ping")
	got := e.send(e2eOwner, allowedG, "allow public ping")
	if len(got) != 1 || !strings.HasPrefix(got[0].Content, "`ping` is now open to everyone, in every server I'm in.\nAnyone who can add me") {
		t.Fatalf("allow public ping: %+v", got)
	}
	e.wantReply(e2eStranger, unlistedG, "ping", "<@9> pong")
	e.send(e2eOwner, allowedG, "deny public ping")
	e.wantSilence(e2eStranger, unlistedG, "ping")
}

// The deny guild reply counts the openings it keeps; none, no note.
func TestE2EDenyGuildMentionsKeptOpenings(t *testing.T) {
	e := newE2E(t)
	e.send(e2eOwner, allowedG, "allow guild")
	e.wantReply(e2eOwner, allowedG, "deny guild", "This server is no longer allowed. Only owners and public commands work there now.")
	e.send(e2eOwner, allowedG, "allow guild")
	e.send(e2eOwner, allowedG, "allow everyone play")
	e.send(e2eOwner, allowedG, "allow everyone ask") // unknown here: refused, not stored
	e.send(e2eOwner, allowedG, "allow public play")  // public: not this server's
	e.send(e2eOwner, unlistedG, "allow everyone skip")
	e.send(e2eOwner, unlistedG, "allow everyone play") // another server's
	e.send(e2eOwner, allowedG, "allow everyone debug") // refused
	e.wantReply(e2eOwner, allowedG, "deny guild", "This server is no longer allowed. Only owners and public commands work there now.\n"+
		"Its open command (`play`) comes back if you allow it again.")
	e.send(e2eOwner, allowedG, "deny public play")
	e.wantSilence(e2eStranger, allowedG, "skip") // denied: the kept opening is inactive
	e.send(e2eOwner, allowedG, "allow guild")
	e.wantReply(e2eStranger, allowedG, "skip", "") // the opening came back (play, inherited by skip)
}

// named is a stand-in command that replies with an empty message.
type named string

func (n named) Name() string { return string(n) }
func (named) Run(ctx context.Context, req commands.Request) error {
	return req.Reply.Reply(ctx, commands.Reply{})
}

// debug is owner-only: it can't be granted, and others get silence.
func TestE2EDebugIsOwnerOnly(t *testing.T) {
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.wantReply(e2eOwner, allowedG, "allow <@8> debug", "`debug` is owner-only and can't be granted.")
	e.wantSilence(e2eFriend, allowedG, "debug access")
	if got := e.send(e2eOwner, allowedG, "debug access"); len(got) != 1 || !strings.Contains(got[0].Content, "**Allowed servers** (1)") {
		t.Errorf("owner's debug access: %+v", got)
	}
}
