package router

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
)

const self snowflake.ID = 1000

func TestParse(t *testing.T) {
	cases := []struct {
		in         string
		name, args string
		reason     string
	}{
		{"<@1000> ping", "ping", "", ""},
		{"<@!1000> ping", "ping", "", ""},
		{"<@&5000> ping", "", "", reasonRole}, // role mentions never trigger, even the bot's own role
		{"<@1000> PiNg", "ping", "", ""},
		{"   <@1000>   ping   ", "ping", "", ""},
		{"<@1000>ping", "ping", "", ""},
		{"<@1000> play https://youtu.be/x  extra", "play", "https://youtu.be/x  extra", ""},
		{"<@1000>\nping\targ", "ping", "arg", ""},
		{"", "", "", reasonEmpty},
		{"   ", "", "", reasonEmpty},
		{"<@1000>", "", "", reasonNoCommand},
		{"<@1000>    ", "", "", reasonNoCommand},
		{"ping", "", "", reasonNoMention},
		{"hey <@1000> ping", "", "", reasonNoMention},
		{"<@abc> ping", "", "", reasonNoMention},
		{"<@1000 ping", "", "", reasonNoMention},
		{"<@2000> ping", "", "", reasonOtherUser},
		{"<@10000> ping", "", "", reasonOtherUser}, // longer ID with our ID as prefix
		{"<@&6000> ping", "", "", reasonRole},
		{"<@&1000> ping", "", "", reasonRole}, // a role ID equal to the bot's user ID is still a role
	}
	for _, c := range cases {
		name, args, reason := parse(c.in, self)
		if name != c.name || args != c.args || reason != c.reason {
			t.Errorf("parse(%q) = (%q, %q, %q), want (%q, %q, %q)", c.in, name, args, reason, c.name, c.args, c.reason)
		}
	}
}

type fakeReplier struct{ got []commands.Reply }

func (f *fakeReplier) Reply(_ context.Context, r commands.Reply) error {
	f.got = append(f.got, r)
	return nil
}

func newRouter(t *testing.T, selfID snowflake.ID) *Router {
	t.Helper()
	reg, err := commands.NewRegistry(commands.Ping{})
	if err != nil {
		t.Fatal(err)
	}
	checker := fakeChecker{owners: []snowflake.ID{7}, public: []string{"ping"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(func() snowflake.ID { return selfID }, reg, checker, logger)
}

func TestHandle(t *testing.T) {
	const owner, stranger snowflake.ID = 7, 8
	cases := []struct {
		name      string
		selfID    snowflake.ID
		msg       Message
		wantReply *commands.Reply
	}{
		{
			name:      "anyone can ping",
			selfID:    self,
			msg:       Message{AuthorID: stranger, Content: "<@1000> ping"},
			wantReply: &commands.Reply{Content: "<@8> pong", Mentions: []snowflake.ID{stranger}},
		},
		{
			name:      "owner ping",
			selfID:    self,
			msg:       Message{AuthorID: owner, Content: "<@!1000> PING"},
			wantReply: &commands.Reply{Content: "<@7> pong", Mentions: []snowflake.ID{owner}},
		},
		{
			name:      "owner unknown command",
			selfID:    self,
			msg:       Message{AuthorID: owner, Content: "<@1000> dance"},
			wantReply: &commands.Reply{Content: "unknown command"},
		},
		{
			name:   "stranger unknown command is silent",
			selfID: self,
			msg:    Message{AuthorID: stranger, Content: "<@1000> dance"},
		},
		{
			name:   "role mention ignored",
			selfID: self,
			msg:    Message{AuthorID: owner, Content: "<@&5000> ping"},
		},
		{
			name:   "bot author ignored",
			selfID: self,
			msg:    Message{AuthorID: stranger, AuthorBot: true, Content: "<@1000> ping"},
		},
		{
			name:   "no mention ignored",
			selfID: self,
			msg:    Message{AuthorID: stranger, Content: "ping"},
		},
		{
			name:   "not ready yet",
			selfID: 0,
			msg:    Message{AuthorID: stranger, Content: "<@1000> ping"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := &fakeReplier{}
			newRouter(t, c.selfID).Handle(context.Background(), c.msg, rep)
			if c.wantReply == nil {
				if len(rep.got) != 0 {
					t.Fatalf("expected no reply, got %+v", rep.got)
				}
				return
			}
			if len(rep.got) != 1 {
				t.Fatalf("got %d replies, want 1", len(rep.got))
			}
			got := rep.got[0]
			if got.Content != c.wantReply.Content || !slices.Equal(got.Mentions, c.wantReply.Mentions) {
				t.Errorf("reply = %+v, want %+v", got, *c.wantReply)
			}
		})
	}
}

func TestHandleDebugLogsReasonNotContent(t *testing.T) {
	reg, _ := commands.NewRegistry(commands.Ping{})
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := New(func() snowflake.ID { return self }, reg, fakeChecker{public: []string{"ping"}}, logger)

	r.Handle(context.Background(), Message{AuthorID: 8, Content: "<@2000> secret-text"}, &fakeReplier{})
	r.Handle(context.Background(), Message{AuthorID: 8, Content: "<@&5000> ping secret-text"}, &fakeReplier{})

	out := buf.String()
	if strings.Contains(out, "secret-text") {
		t.Errorf("log leaked message content:\n%s", out)
	}
	for _, want := range []string{reasonOtherUser, reasonRole} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing reason %q:\n%s", want, out)
		}
	}
}

// fakeChecker is a minimal access.Checker: owners run anything, anyone runs public commands.
type fakeChecker struct {
	owners []snowflake.ID
	public []string
}

func (f fakeChecker) Allowed(_ context.Context, user, _ snowflake.ID, command string) bool {
	return slices.Contains(f.owners, user) || slices.Contains(f.public, command)
}
