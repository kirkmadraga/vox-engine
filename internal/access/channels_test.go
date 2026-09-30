package access_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
)

// The channel rules for ask are the bot operator's deliberate choices; the
// test names say which. See the Policy doc comment.

const (
	askChan   snowflake.ID = 500
	otherChan snowflake.ID = 600
)

func newChannelPolicy(b access.Backend, logger *slog.Logger) *access.Policy {
	return access.NewPolicy(b, access.Options{
		Owners:         []snowflake.ID{owner},
		Public:         []string{"ping"},
		OwnerOnly:      []string{"allow", "deny", "access"},
		ChannelLimited: []string{"Ask"},
		Logger:         logger,
	})
}

func TestAskIsDefaultDeny(t *testing.T) {
	ctx := context.Background()
	p := newChannelPolicy(accesstest.NewMemory(), nil)
	p.AllowGuild(ctx, guildA, owner)
	p.Grant(ctx, friend, "ask", owner)
	// Allowed server and a grant, but no channel allowed yet: nowhere.
	if !p.Allowed(ctx, friend, guildA, "ask") {
		t.Fatal("setup: the guild and grant rules should pass")
	}
	if p.ChannelAllowed(ctx, friend, "ask", askChan) {
		t.Error("with no ask channels, ask must be allowed nowhere")
	}
}

func TestAskOnlyInAllowedChannels(t *testing.T) {
	ctx := context.Background()
	p := newChannelPolicy(accesstest.NewMemory(), nil)
	if _, err := p.AllowChannel(ctx, askChan, "ASK", owner); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		command  string
		channels []snowflake.ID
		want     bool
	}{
		{"allowed channel", "ask", []snowflake.ID{askChan}, true},
		{"case-insensitive command", "Ask", []snowflake.ID{askChan}, true},
		{"other channel", "ask", []snowflake.ID{otherChan}, false},
		{"thread whose parent is allowed", "ask", []snowflake.ID{otherChan, askChan}, true},
		{"thread with no known parent", "ask", []snowflake.ID{otherChan, 0}, false},
		{"no channel at all", "ask", nil, false},
		{"commands without limits run anywhere", "play", []snowflake.ID{otherChan}, true},
	}
	for _, c := range cases {
		if got := p.ChannelAllowed(ctx, friend, c.command, c.channels...); got != c.want {
			t.Errorf("%s: ChannelAllowed = %v, want %v", c.name, got, c.want)
		}
	}
	// Allowing a channel for ask says nothing about other commands' channels,
	// and doesn't replace the guild and grant rules.
	if p.Allowed(ctx, friend, guildA, "ask") {
		t.Error("an allowed channel alone must not let a user without a grant in an unlisted guild run ask")
	}
}

func TestAskChannelLimitsDontApplyToOwners(t *testing.T) {
	p := newChannelPolicy(accesstest.NewMemory(), nil)
	if !p.ChannelAllowed(context.Background(), owner, "ask", otherChan) {
		t.Error("owners bypass channel limits")
	}
}

func TestAskChannelDenyRemovesIt(t *testing.T) {
	ctx := context.Background()
	p := newChannelPolicy(accesstest.NewMemory(), nil)
	p.AllowChannel(ctx, askChan, "ask", owner)
	if changed, err := p.DenyChannel(ctx, askChan, "ask", owner); err != nil || !changed {
		t.Fatalf("DenyChannel = %v, %v", changed, err)
	}
	if p.ChannelAllowed(ctx, friend, "ask", askChan) {
		t.Error("still allowed after deny")
	}
}

func TestAskChannelChangesAreLogged(t *testing.T) {
	var buf bytes.Buffer
	p := newChannelPolicy(accesstest.NewMemory(), slog.New(slog.NewTextHandler(&buf, nil)))
	p.AllowChannel(context.Background(), askChan, "ask", owner)
	if out := buf.String(); !strings.Contains(out, "allow channel") || !strings.Contains(out, "channel=500") {
		t.Errorf("log: %s", out)
	}
}

func TestAskChannelBackendErrorsFailClosed(t *testing.T) {
	var buf bytes.Buffer
	p := newChannelPolicy(failingBackend{accesstest.NewMemory()}, slog.New(slog.NewTextHandler(&buf, nil)))
	if p.ChannelAllowed(context.Background(), friend, "ask", askChan) {
		t.Error("backend error must deny")
	}
	if !strings.Contains(buf.String(), "storage down") {
		t.Errorf("backend errors should be logged:\n%s", buf.String())
	}
}

// A command that shares ask's access (forget) also runs only where ask runs.
func TestAskChannelsCoverInheritingCommands(t *testing.T) {
	ctx := context.Background()
	p := access.NewPolicy(accesstest.NewMemory(), access.Options{
		Owners: []snowflake.ID{owner}, ChannelLimited: []string{"ask"}, Inherit: map[string]string{"forget": "ask"},
	})
	p.AllowChannel(ctx, askChan, "ask", owner)
	if !p.ChannelAllowed(ctx, friend, "forget", askChan) || p.ChannelAllowed(ctx, friend, "forget", otherChan) {
		t.Error("forget must follow ask's channels")
	}
}
