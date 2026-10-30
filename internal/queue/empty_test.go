package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

const (
	emptyMsg = "msg 1: The queue is empty."
	leftMsg  = "msg 1: Nothing's queued, so I left voice."
)

// The last track ending says so at once, the bot waits, and it says so again
// once it has left.
func TestEmptyThenWaitThenLeft(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, emptyMsg)
	h.rec.waitFor(t, "idle timer")
	h.never(t, leftMsg)
	h.timer <- time.Now()
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, leftMsg)
}

func TestNewTrackWhileWaitingStaysQuiet(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.conn.release(1)
	h.rec.waitFor(t, "idle timer")
	h.play(t, 1, "b")
	h.rec.waitFor(t, "msg 1: Now playing: **b**")
	h.rec.waitFor(t, "idle timer")
	h.never(t, leftMsg)
	if n := count(h.rec.all(), emptyMsg); n != 2 {
		t.Errorf("said empty %d times, want once per time it ran out: %q", n, h.rec.all())
	}
}

func TestEmptyOnlyAfterTheLastTrack(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.play(t, 1, "b")
	h.conn.release(1)
	h.rec.waitFor(t, "msg 1: Now playing: **b**")
	h.rec.waitFor(t, emptyMsg)
	if n := count(h.rec.all(), emptyMsg); n != 1 {
		t.Errorf("said empty %d times: %q", n, h.rec.all())
	}
}

func TestSkippingTheLastTrackSaysEmpty(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	if _, ok := h.m.Skip(1); !ok {
		t.Fatal("nothing to skip")
	}
	h.conn.release(1)
	h.rec.waitFor(t, emptyMsg)
}

// Stop, being disconnected and shutting down say so themselves, or need no
// saying.
func TestQuietWhenMadeToLeave(t *testing.T) {
	leave := map[string]func(h *slotHarness){
		"stop":     func(h *slotHarness) { h.m.Stop(1) },
		"kick":     func(h *slotHarness) { h.m.Disconnected(1) },
		"shutdown": func(h *slotHarness) { h.cancel() },
	}
	for name, do := range leave {
		t.Run(name+" while playing", func(t *testing.T) {
			h := newSlotHarness(t, 1, 2*time.Minute)
			h.play(t, 1, "a")
			h.rec.waitFor(t, "holding 1")
			do(h)
			h.conn.release(1)
			h.rec.waitFor(t, "leave 100")
			h.never(t, emptyMsg)
			h.never(t, leftMsg)
		})
		t.Run(name+" while waiting", func(t *testing.T) {
			h := newSlotHarness(t, 1, 2*time.Minute)
			h.play(t, 1, "a")
			h.rec.waitFor(t, "holding 1")
			h.conn.release(1)
			h.rec.waitFor(t, "idle timer")
			do(h)
			h.rec.waitFor(t, "leave 100")
			h.never(t, leftMsg)
		})
	}
}

// With another server waiting there's no wait, so one message says it all.
func TestOtherServerWaitingSaysOnlyLeft(t *testing.T) {
	h := newSlotHarness(t, 1, 2*time.Minute)
	h.play(t, 1, "a")
	h.rec.waitFor(t, "holding 1")
	h.play(t, 2, "b")
	h.conn.release(1)
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, leftMsg)
	h.never(t, emptyMsg)
}

// playFailing queues a track that fails to load once the worker has taken it,
// so it fails as "Couldn't play" rather than being dropped from the queue.
func playFailing(t *testing.T, h *slotHarness) {
	t.Helper()
	gate := make(chan struct{})
	if _, err := h.m.Enqueue(1, Track{URL: "https://youtu.be/bad", VoiceChannel: 100, TextChannel: 1, Load: load("bad", gate, userErr{"nope"})}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); h.m.List(1).Current == nil; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the worker never took the track")
		}
	}
	close(gate)
	h.rec.waitFor(t, "msg 1: Couldn't play")
}

func TestFailedLastTrackIsNotCalledEmpty(t *testing.T) {
	t.Run("in voice", func(t *testing.T) {
		h := newSlotHarness(t, 1, 2*time.Minute)
		h.play(t, 1, "a")
		h.rec.waitFor(t, "holding 1")
		h.conn.release(1)
		h.rec.waitFor(t, "idle timer")
		playFailing(t, h)
		h.rec.waitFor(t, "idle timer")
		if n := count(h.rec.all(), emptyMsg); n != 1 {
			t.Errorf("said empty %d times, want only for the track that played: %q", n, h.rec.all())
		}
		h.timer <- time.Now()
		h.rec.waitFor(t, "leave 100")
		h.rec.waitFor(t, leftMsg)
	})
	t.Run("never joined", func(t *testing.T) {
		h := newSlotHarness(t, 1, 2*time.Minute)
		playFailing(t, h)
		h.never(t, emptyMsg)
		h.never(t, leftMsg)
	})
}

// gateNotifier holds up the empty message, as a slow Discord call would.
type gateNotifier struct {
	rec  *recorder
	gate chan struct{}
}

func (n gateNotifier) Notify(ch snowflake.ID, content string) {
	n.rec.add(fmt.Sprintf("msg %d: %s", ch, content))
	if content == "The queue is empty." {
		<-n.gate
	}
}

// Stop isn't lost while the empty message is being sent.
func TestStopWhileSayingEmpty(t *testing.T) {
	rec := newRecorder()
	gate := make(chan struct{})
	var open sync.Once
	release := func() { open.Do(func() { close(gate) }) }
	ctx, cancel := context.WithCancel(context.Background())
	m := New(ctx, Options{
		Connector:   &fakeConnector{rec: rec},
		Clock:       instantClock{},
		Notifier:    gateNotifier{rec: rec, gate: gate},
		MaxLength:   3,
		IdleTimeout: time.Minute,
		After: func(time.Duration) <-chan time.Time {
			rec.add("idle timer")
			return nil
		},
	})
	t.Cleanup(func() { release(); cancel(); m.Wait() })

	if _, err := m.Enqueue(1, track("one", 100, load("one", nil, nil))); err != nil {
		t.Fatal(err)
	}
	rec.waitFor(t, "msg 7: The queue is empty.")
	if !m.Stop(1) {
		t.Error("Stop was lost while the message was being sent")
	}
	release()
	rec.waitFor(t, "leave 100")
	if n := count(rec.all(), "idle timer"); n != 0 {
		t.Errorf("waited after a stop: %q", rec.all())
	}
}
