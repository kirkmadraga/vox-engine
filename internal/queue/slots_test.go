package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/voice"
)

// slotConnector gives each guild its own playback hold, so tests can keep one
// guild playing while another finishes. Guild g always joins channel g*100.
type slotConnector struct {
	rec   *recorder
	mu    sync.Mutex
	holds map[snowflake.ID]chan struct{}
}

func (c *slotConnector) hold(g snowflake.ID) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.holds[g]
	if !ok {
		h = make(chan struct{})
		c.holds[g] = h
	}
	return h
}

// release lets guild g's current track finish (like a real connection that
// stops accepting audio after a kick or stop). Safe to call twice.
func (c *slotConnector) release(g snowflake.ID) {
	h := c.hold(g)
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-h:
	default:
		close(h)
	}
}

func (c *slotConnector) releaseAll() {
	c.mu.Lock()
	var gs []snowflake.ID
	for g := range c.holds {
		gs = append(gs, g)
	}
	c.mu.Unlock()
	for _, g := range gs {
		c.release(g)
	}
}

func (c *slotConnector) Connect(_ context.Context, g, ch snowflake.ID) (voice.Connection, error) {
	c.rec.add(fmt.Sprintf("join %d/%d", g, ch))
	return &slotConn{rec: c.rec, guild: g, channel: ch, hold: c.hold(g)}, nil
}

type slotConn struct {
	rec     *recorder
	guild   snowflake.ID
	channel snowflake.ID
	hold    chan struct{}
	once    sync.Once
}

func (c *slotConn) WaitReady(context.Context) error         { return nil }
func (c *slotConn) SetSpeaking(context.Context, bool) error { return nil }
func (c *slotConn) Close(context.Context)                   { c.rec.add(fmt.Sprintf("leave %d", c.channel)) }
func (c *slotConn) WriteOpus([]byte) error {
	c.once.Do(func() { c.rec.add(fmt.Sprintf("holding %d", c.guild)); <-c.hold })
	return nil
}

type slotHarness struct {
	m      *Manager
	rec    *recorder
	conn   *slotConnector
	cancel context.CancelFunc
	timer  chan time.Time // the idle timer; send to make it fire
}

func newSlotHarness(t *testing.T, sessions int, idle time.Duration) *slotHarness {
	t.Helper()
	rec := newRecorder()
	h := &slotHarness{rec: rec, conn: &slotConnector{rec: rec, holds: map[snowflake.ID]chan struct{}{}}, timer: make(chan time.Time, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.m = New(ctx, Options{
		Connector:   h.conn,
		Clock:       instantClock{},
		Notifier:    fakeNotifier{rec: rec},
		MaxLength:   5,
		MaxSessions: sessions,
		IdleTimeout: idle,
		After: func(d time.Duration) <-chan time.Time {
			rec.add(fmt.Sprintf("idle timer %v", d))
			return h.timer
		},
	})
	t.Cleanup(func() { h.conn.releaseAll(); cancel(); h.m.Wait() })
	return h
}

// play queues a track in guild g (voice channel g*100, text channel g).
func (h *slotHarness) play(t *testing.T, g snowflake.ID, title string) Position {
	t.Helper()
	pos, err := h.m.Enqueue(g, Track{URL: "https://youtu.be/" + title, RequestedBy: 42, VoiceChannel: g * 100, TextChannel: g, Load: load(title, nil, nil)})
	if err != nil {
		t.Fatalf("enqueue %s in %d: %v", title, g, err)
	}
	return pos
}

// never fails if an event with prefix shows up within a short while.
func (h *slotHarness) never(t *testing.T, prefix string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	for _, e := range h.rec.all() {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			t.Fatalf("unexpected %q; events: %q", e, h.rec.all())
		}
	}
}

func count(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func TestTwoGuildsPlayAtOnceThirdWaits(t *testing.T) {
	h := newSlotHarness(t, 2, 0)
	if pos := h.play(t, 1, "a"); pos.OtherGuild {
		t.Error("guild 1 must not wait")
	}
	h.rec.waitFor(t, "holding 1")
	if pos := h.play(t, 2, "b"); pos.OtherGuild {
		t.Error("guild 2 must not wait: a second slot is free")
	}
	h.rec.waitFor(t, "holding 2")

	if pos := h.play(t, 3, "c"); !pos.OtherGuild {
		t.Error("guild 3 must be told it waits: both slots are playing")
	}
	h.never(t, "join 3/")

	h.conn.release(1)
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, "join 3/300") // guild 1's slot went to guild 3
	h.conn.release(2)
	h.conn.release(3)
	h.rec.waitFor(t, "leave 200")
	h.rec.waitFor(t, "leave 300")
}

func TestKickStopSkipOnlyAffectTheirGuild(t *testing.T) {
	h := newSlotHarness(t, 2, 0)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.play(t, 2, "b")
	h.play(t, 2, "b2")
	h.rec.waitFor(t, "holding 2")

	if !h.m.Disconnected(1) {
		t.Fatal("kick in guild 1 not handled")
	}
	h.conn.release(1)
	h.rec.waitFor(t, "leave 100")
	if l := h.m.List(2); l.Current == nil || len(l.Upcoming) != 1 {
		t.Errorf("guild 2's queue changed after a kick in guild 1: %+v", l)
	}
	h.never(t, "leave 200")

	// A new guild gets the freed slot while guild 2 keeps playing.
	h.play(t, 3, "c")
	h.rec.waitFor(t, "holding 3")
	if !h.m.Stop(3) {
		t.Fatal("stop in guild 3")
	}
	h.conn.release(3)
	h.rec.waitFor(t, "leave 300")
	if l := h.m.List(2); l.Current == nil || len(l.Upcoming) != 1 {
		t.Errorf("guild 2's queue changed after stop in guild 3: %+v", l)
	}
	h.conn.release(2)
	h.rec.waitFor(t, "leave 200")
}

// An idle guild holds no slot another guild is waiting for.
func TestActiveGuildsFreeTheirSlot(t *testing.T) {
	h := newSlotHarness(t, 1, 0)
	for i, g := range []snowflake.ID{1, 2, 3} {
		h.play(t, g, fmt.Sprint(g))
		h.rec.waitFor(t, fmt.Sprintf("holding %d", g))
		h.conn.release(g)
		h.rec.waitFor(t, fmt.Sprintf("leave %d", g*100))
		if i < 2 && count(h.rec.all(), "join") != i+1 {
			t.Errorf("joins = %q", h.rec.all())
		}
	}
}

func TestIdleStaysThenLeaves(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer 2m0s")
	h.never(t, "leave")
	h.timer <- time.Now()
	h.rec.waitFor(t, "leave 100")
}

func TestIdleNewTrackPlaysWithoutRejoin(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer")

	h.play(t, 1, "b")
	h.rec.waitFor(t, "msg 1: Now playing: **b**") // conn's hold is already released, so b plays through
	h.rec.waitFor(t, "idle timer")                // idle again after b, with a fresh timer
	if n := count(h.rec.all(), "join"); n != 1 {
		t.Errorf("joined %d times, want 1 (no rejoin while idle): %q", n, h.rec.all())
	}
	h.timer <- time.Now()
	h.rec.waitFor(t, "leave 100")
}

func TestStopWhileIdleLeaves(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer")
	if !h.m.Stop(1) {
		t.Error("stop while idle must report that it did something")
	}
	h.rec.waitFor(t, "leave 100")
	if h.m.Stop(1) {
		t.Error("stop after leaving: nothing to stop")
	}
}

// stop while playing leaves right after, without idling.
func TestStopWhilePlayingSkipsIdle(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.m.Stop(1)
	h.conn.release(1)
	h.rec.waitFor(t, "leave 100")
	if count(h.rec.all(), "idle timer") != 0 {
		t.Errorf("idled after stop: %q", h.rec.all())
	}
}

func TestKickWhileIdleFreesSlot(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer")
	if !h.m.Disconnected(1) {
		t.Fatal("kick while idle not handled")
	}
	h.rec.waitFor(t, "leave 100")
	h.play(t, 2, "b")
	h.rec.waitFor(t, "join 2/200")
}

// With one slot, an idle guild hands it to a guild that wants to play.
func TestWaitingGuildTakesIdleSlot(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer")

	if pos := h.play(t, 2, "b"); pos.OtherGuild {
		t.Error("an idle guild must not count as busy")
	}
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, "join 2/200")
}

// A guild whose queue ends while another guild waits leaves at once.
func TestNoIdleWhenAnotherGuildWaits(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	if pos := h.play(t, 2, "b"); !pos.OtherGuild {
		t.Error("guild 2 must be told it waits")
	}
	h.conn.release(1)
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, "join 2/200")
	if count(h.rec.all(), "idle timer") != 0 {
		t.Errorf("guild 1 idled while guild 2 waited: %q", h.rec.all())
	}
}

func TestShutdownWhileIdleLeaves(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer")
	h.cancel()
	h.rec.waitFor(t, "leave 100")
	h.m.Wait()
}
