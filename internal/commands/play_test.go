package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"bot/internal/voice"
)

type fakeLocator map[snowflake.ID]snowflake.ID // user -> voice channel

func (f fakeLocator) UserVoiceChannel(_, user snowflake.ID) (snowflake.ID, bool) {
	ch, ok := f[user]
	return ch, ok
}

type fakePlayer struct {
	startErr error
	doneErr  error // passed to done, asynchronously
	started  []string
}

func (f *fakePlayer) Start(g, ch snowflake.ID, path string, done func(error)) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, fmt.Sprintf("%d/%d %s", g, ch, path))
	go done(f.doneErr)
	return nil
}

// syncReplier collects replies from both the command and the background callback.
type syncReplier struct {
	mu  sync.Mutex
	got []Reply
	ch  chan struct{}
}

func newSyncReplier() *syncReplier { return &syncReplier{ch: make(chan struct{}, 10)} }

func (s *syncReplier) Reply(_ context.Context, r Reply) error {
	s.mu.Lock()
	s.got = append(s.got, r)
	s.mu.Unlock()
	s.ch <- struct{}{}
	return nil
}

// wait returns once n replies have arrived.
func (s *syncReplier) wait(t *testing.T, n int) []Reply {
	t.Helper()
	for range n {
		select {
		case <-s.ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for reply")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reply(nil), s.got...)
}

func runPlay(t *testing.T, p Play, author snowflake.ID, args string) *syncReplier {
	t.Helper()
	rep := newSyncReplier()
	if err := p.Run(context.Background(), Request{GuildID: 1, AuthorID: author, Args: args, Reply: rep}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func TestPlayStartsToneInCallersChannel(t *testing.T) {
	player := &fakePlayer{}
	p := Play{Voice: fakeLocator{5: 77}, Player: player, TonePath: "cache/tone.opus"}
	got := runPlay(t, p, 5, "").wait(t, 1)
	if got[0].Content != "Playing the test tone in <#77>." {
		t.Errorf("reply = %q", got[0].Content)
	}
	if len(player.started) != 1 || player.started[0] != "1/77 cache/tone.opus" {
		t.Errorf("started = %v", player.started)
	}
}

func TestPlayRefusals(t *testing.T) {
	cases := []struct {
		name   string
		player *fakePlayer
		author snowflake.ID
		args   string
		want   string
	}{
		{"not in voice", &fakePlayer{}, 6, "", "Join a voice channel first"},
		{"url not yet", &fakePlayer{}, 5, "https://youtu.be/x", "Only the test tone works for now"},
		{"busy", &fakePlayer{startErr: voice.ErrBusy}, 5, "", "already playing"},
		{"missing tone", &fakePlayer{startErr: fmt.Errorf("open: %w", fs.ErrNotExist)}, 5, "", "test tone file is missing"},
		{"other start error", &fakePlayer{startErr: errors.New("bad ogg")}, 5, "", "Couldn't start playback"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Play{Voice: fakeLocator{5: 77}, Player: c.player, TonePath: "t.opus"}
			got := runPlay(t, p, c.author, c.args).wait(t, 1)
			if !strings.Contains(got[0].Content, c.want) {
				t.Errorf("reply = %q, want containing %q", got[0].Content, c.want)
			}
			if len(got[0].Mentions) != 0 {
				t.Error("play replies must not ping")
			}
			if c.name == "not in voice" || c.name == "url not yet" {
				if len(c.player.started) != 0 {
					t.Error("player must not start")
				}
			}
		})
	}
}

func TestPlayReportsBackgroundFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"dave":    {fmt.Errorf("x: %w", voice.ErrNotReady), "encryption (DAVE)"},
		"generic": {errors.New("udp closed"), "Playback failed"},
	} {
		t.Run(name, func(t *testing.T) {
			p := Play{Voice: fakeLocator{5: 77}, Player: &fakePlayer{doneErr: tc.err}, TonePath: "t.opus"}
			got := runPlay(t, p, 5, "").wait(t, 2)
			var found bool
			for _, r := range got {
				found = found || strings.Contains(r.Content, tc.want)
			}
			if !found {
				t.Errorf("replies %+v missing %q", got, tc.want)
			}
		})
	}
}

func TestPlayQuietOnSuccessOrShutdown(t *testing.T) {
	for _, doneErr := range []error{nil, context.Canceled} {
		p := Play{Voice: fakeLocator{5: 77}, Player: &fakePlayer{doneErr: doneErr}, TonePath: "t.opus"}
		rep := runPlay(t, p, 5, "")
		rep.wait(t, 1) // "Playing..."
		select {
		case <-rep.ch:
			t.Errorf("done(%v): unexpected extra reply %+v", doneErr, rep.got)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
