package discordio

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
)

type fakeTyping struct {
	mu   sync.Mutex
	sent []snowflake.ID
}

func (f *fakeTyping) SendTyping(ch snowflake.ID, _ ...rest.RequestOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, ch)
	return nil
}

func (f *fakeTyping) count(ch snowflake.ID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.sent {
		if c == ch {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// One indicator per channel: a second waiter doesn't send another; stopping
// one while another waits sends it again (the answer just cleared it); the
// last stop ends it.
func TestChannelTypingSharedPerChannel(t *testing.T) {
	f := &fakeTyping{}
	ty := &ChannelTyping{Sender: f, Interval: time.Hour} // no ticks: only the sends we cause
	stopA := ty.Start(10)
	waitFor(t, "the first send", func() bool { return f.count(10) == 1 })
	stopB := ty.Start(10)
	time.Sleep(20 * time.Millisecond)
	if n := f.count(10); n != 1 {
		t.Fatalf("a second waiter sent again: %d sends", n)
	}

	stopA()
	if n := f.count(10); n != 2 {
		t.Errorf("after the first answer, want typing re-sent (2 sends), got %d", n)
	}
	stopA() // a second call is ignored
	stopB()
	time.Sleep(20 * time.Millisecond)
	if n := f.count(10); n != 2 {
		t.Errorf("after the last stop, want no more sends, got %d", n)
	}
	if f.count(20) != 0 {
		t.Error("another channel must never see typing")
	}
}

func TestChannelTypingKeepsItAlive(t *testing.T) {
	f := &fakeTyping{}
	ty := &ChannelTyping{Sender: f, Interval: 5 * time.Millisecond}
	stop := ty.Start(10)
	waitFor(t, "repeated sends", func() bool { return f.count(10) >= 3 })
	stop()
	time.Sleep(20 * time.Millisecond)
	n := f.count(10)
	time.Sleep(30 * time.Millisecond)
	if f.count(10) != n {
		t.Error("typing continued after the last stop")
	}
}

func TestChannelReplierTyping(t *testing.T) {
	f := &fakeTyping{}
	var ti commands.TypingIndicator = ChannelReplier{ChannelID: 10, Typing: &ChannelTyping{Sender: f, Interval: time.Hour}}
	stop := ti.StartTyping()
	waitFor(t, "a send", func() bool { return f.count(10) == 1 })
	stop()
	// Without a typing manager it's a no-op.
	ChannelReplier{ChannelID: 10}.StartTyping()()
	if !slices.Equal(f.sent, []snowflake.ID{10}) {
		t.Errorf("sent %v", f.sent)
	}
}
