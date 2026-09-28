package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"bot/internal/voice"
)

const fixture = "../voice/testdata/tone-1s.opus"

// instantClock makes pacing free, so a 1 s fixture "plays" instantly.
type instantClock struct{}

func (instantClock) Now() time.Time { return time.Unix(0, 0) }
func (instantClock) Sleep(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}

// recorder is a thread-safe event log shared by all fakes, so tests can assert ordering.
type recorder struct {
	mu     sync.Mutex
	events []string
	signal chan string
}

func newRecorder() *recorder { return &recorder{signal: make(chan string, 100)} }

func (r *recorder) add(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	r.signal <- e
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// waitFor blocks until an event with the given prefix is recorded.
func (r *recorder) waitFor(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-r.signal:
			if strings.HasPrefix(e, prefix) {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; events so far: %q", prefix, r.all())
		}
	}
}

type fakeConn struct {
	rec     *recorder
	channel snowflake.ID
	hold    chan struct{} // if non-nil, the first WriteOpus waits here (keeps a track "playing")
	once    sync.Once
	onClose func()
}

func (c *fakeConn) WaitReady(context.Context) error         { return nil }
func (c *fakeConn) SetSpeaking(context.Context, bool) error { return nil }
func (c *fakeConn) Close(context.Context) {
	c.rec.add(fmt.Sprintf("leave %d", c.channel))
	if c.onClose != nil {
		c.onClose()
	}
}
func (c *fakeConn) WriteOpus([]byte) error {
	if c.hold != nil {
		c.once.Do(func() { c.rec.add("holding"); <-c.hold })
	}
	return nil
}

type fakeConnector struct {
	rec     *recorder
	hold    chan struct{}
	failFor snowflake.ID // channel whose join fails
	onClose func()
}

func (f *fakeConnector) Connect(_ context.Context, g, ch snowflake.ID) (voice.Connection, error) {
	if ch == f.failFor {
		return nil, errors.New("missing Connect permission")
	}
	f.rec.add(fmt.Sprintf("join %d/%d", g, ch))
	return &fakeConn{rec: f.rec, channel: ch, hold: f.hold, onClose: f.onClose}, nil
}

type fakeNotifier struct{ rec *recorder }

func (n fakeNotifier) Notify(ch snowflake.ID, content string) {
	n.rec.add(fmt.Sprintf("msg %d: %s", ch, content))
}

type userErr struct{ msg string }

func (e userErr) Error() string       { return "internal: " + e.msg }
func (e userErr) UserMessage() string { return e.msg }

// load returns a LoadFunc for a titled track, optionally gated or failing.
func load(title string, gate chan struct{}, err error) LoadFunc {
	return func(ctx context.Context) (Loaded, error) {
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return Loaded{}, ctx.Err()
			}
		}
		if err != nil {
			return Loaded{}, err
		}
		return Loaded{Path: fixture, Title: title, Duration: 3*time.Minute + 5*time.Second}, nil
	}
}

func track(title string, voiceCh snowflake.ID, l LoadFunc) Track {
	return Track{URL: "https://youtu.be/" + title, RequestedBy: 42, VoiceChannel: voiceCh, TextChannel: 7, Load: l}
}

type harness struct {
	m      *Manager
	rec    *recorder
	cancel context.CancelFunc
}

func newHarness(t *testing.T, hold chan struct{}, failFor snowflake.ID) harness {
	t.Helper()
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	m := New(ctx, Options{
		Connector: &fakeConnector{rec: rec, hold: hold, failFor: failFor},
		Clock:     instantClock{},
		Notifier:  fakeNotifier{rec: rec},
		MaxLength: 3,
	})
	t.Cleanup(func() { cancel(); m.Wait() })
	return harness{m: m, rec: rec, cancel: cancel}
}

func TestPlaysInOrderThenLeaves(t *testing.T) {
	h := newHarness(t, nil, 0)
	gate := make(chan struct{})
	for i, title := range []string{"one", "two", "three"} {
		pos, err := h.m.Enqueue(1, track(title, 100, load(title, gate, nil)))
		if err != nil || pos.Ahead != i || pos.OtherGuild {
			t.Fatalf("enqueue %s: pos=%+v err=%v", title, pos, err)
		}
	}
	close(gate)
	h.rec.waitFor(t, "leave")
	want := []string{
		"join 1/100",
		"msg 7: Now playing: **one** (3:05), requested by <@42>.",
		"msg 7: Now playing: **two** (3:05), requested by <@42>.",
		"msg 7: Now playing: **three** (3:05), requested by <@42>.",
		"leave 100",
	}
	if got := h.rec.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Idle again: a new request starts a fresh session.
	h.m.Enqueue(1, track("four", 100, load("four", nil, nil)))
	h.rec.waitFor(t, "msg 7: Now playing: **four**")
	h.rec.waitFor(t, "leave")
}

func TestQueueFull(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, nil, 0)
	for i := range 3 {
		if _, err := h.m.Enqueue(1, track(fmt.Sprint(i), 100, load("x", gate, nil))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.m.Enqueue(1, track("overflow", 100, load("x", gate, nil))); !errors.Is(err, ErrFull) {
		t.Errorf("err = %v, want ErrFull", err)
	}
	// Other guilds have their own limit.
	if _, err := h.m.Enqueue(2, track("other", 200, load("x", gate, nil))); err != nil {
		t.Errorf("other guild: %v", err)
	}
	close(gate)
}

func TestSkipPlaysNext(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	if _, ok := h.m.Skip(1); ok {
		t.Error("skip with nothing playing should report false")
	}
	h.m.Enqueue(1, track("one", 100, load("one", nil, nil)))
	h.m.Enqueue(1, track("two", 100, load("two", nil, nil)))
	h.rec.waitFor(t, "holding")

	skipped, ok := h.m.Skip(1)
	if !ok || skipped.Title != "one" {
		t.Fatalf("Skip = %+v, %v", skipped, ok)
	}
	close(hold)
	h.rec.waitFor(t, "msg 7: Now playing: **two**")
	h.rec.waitFor(t, "leave")
}

func TestStopClearsAndLeaves(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	if h.m.Stop(1) {
		t.Error("stop with nothing queued should report false")
	}
	h.m.Enqueue(1, track("one", 100, load("one", nil, nil)))
	h.m.Enqueue(1, track("two", 100, load("two", nil, nil)))
	h.rec.waitFor(t, "holding")
	if !h.m.Stop(1) {
		t.Fatal("Stop reported nothing to stop")
	}
	close(hold)
	h.rec.waitFor(t, "leave")
	for _, e := range h.rec.all() {
		if strings.Contains(e, "**two**") {
			t.Errorf("stopped track played: %q", e)
		}
	}
	if l := h.m.List(1); l.Current != nil || len(l.Upcoming) != 0 {
		t.Errorf("queue not empty after stop: %+v", l)
	}
}

func TestListShowsTitlesOnceLoaded(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	slow := make(chan struct{})
	h.m.Enqueue(1, track("one", 100, load("One", nil, nil)))
	h.m.Enqueue(1, track("two", 100, load("Two", slow, nil)))
	h.rec.waitFor(t, "holding")

	l := h.m.List(1)
	if l.Current == nil || l.Current.Title != "One" || l.Current.Duration == 0 {
		t.Errorf("current = %+v", l.Current)
	}
	if len(l.Upcoming) != 1 || l.Upcoming[0].Title != "" || l.Upcoming[0].Name() != "<https://youtu.be/two>" {
		t.Errorf("upcoming before load = %+v", l.Upcoming)
	}
	close(slow)
	time.Sleep(20 * time.Millisecond)
	if l := h.m.List(1); len(l.Upcoming) != 1 || l.Upcoming[0].Title != "Two" {
		t.Errorf("upcoming after load = %+v", l.Upcoming)
	}
	close(hold)
}

func TestLoadFailureWhileWaitingIsReportedAndRemoved(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	h.m.Enqueue(1, track("one", 100, load("one", nil, nil)))
	h.m.Enqueue(1, track("private", 100, load("", nil, userErr{"That video is private."})))
	h.m.Enqueue(1, track("three", 100, load("three", nil, nil)))
	msg := h.rec.waitFor(t, "msg 7: Couldn't add")
	if msg != "msg 7: Couldn't add <https://youtu.be/private>: That video is private." {
		t.Errorf("message = %q", msg)
	}
	if l := h.m.List(1); len(l.Upcoming) != 1 {
		t.Errorf("failed track still queued: %+v", l.Upcoming)
	}
	close(hold)
	h.rec.waitFor(t, "msg 7: Now playing: **three**")
	h.rec.waitFor(t, "leave")
	if n := strings.Count(strings.Join(h.rec.all(), "\n"), "Couldn't"); n != 1 {
		t.Errorf("failure reported %d times, want once", n)
	}
}

func TestLoadFailureOfNextTrackReportedOnce(t *testing.T) {
	h := newHarness(t, nil, 0)
	gate := make(chan struct{})
	h.m.Enqueue(1, track("bad", 100, load("", gate, errors.New("disk full"))))
	time.Sleep(20 * time.Millisecond) // the worker is now waiting on this track's load
	close(gate)
	msg := h.rec.waitFor(t, "msg 7: Couldn't")
	if msg != "msg 7: Couldn't play <https://youtu.be/bad>: something went wrong (details are in the bot's log)." {
		t.Errorf("message = %q", msg)
	}
	time.Sleep(20 * time.Millisecond)
	if n := strings.Count(strings.Join(h.rec.all(), "\n"), "Couldn't"); n != 1 {
		t.Errorf("reported %d times: %q", n, h.rec.all())
	}
}

func TestJoinFailureSkipsTrack(t *testing.T) {
	h := newHarness(t, nil, 666)
	h.m.Enqueue(1, track("one", 666, load("one", nil, nil)))
	h.m.Enqueue(1, track("two", 100, load("two", nil, nil)))
	h.rec.waitFor(t, "msg 7: Couldn't join the voice channel, so I skipped **one**.")
	h.rec.waitFor(t, "msg 7: Now playing: **two**")
	h.rec.waitFor(t, "leave 100")
}

func TestDifferentVoiceChannelRejoins(t *testing.T) {
	h := newHarness(t, nil, 0)
	gate := make(chan struct{})
	h.m.Enqueue(1, track("one", 100, load("one", gate, nil)))
	h.m.Enqueue(1, track("two", 101, load("two", gate, nil)))
	close(gate)
	h.rec.waitFor(t, "join 1/100")
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, "join 1/101")
	h.rec.waitFor(t, "leave 101")
}

func TestOneGuildInVoiceAtATime(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	h.m.Enqueue(1, track("a", 100, load("a", nil, nil)))
	h.rec.waitFor(t, "holding")

	pos, err := h.m.Enqueue(2, track("b", 200, load("b", nil, nil)))
	if err != nil || pos.Ahead != 0 || !pos.OtherGuild {
		t.Fatalf("guild 2 enqueue: %+v, %v", pos, err)
	}
	close(hold)
	h.rec.waitFor(t, "leave 100")
	h.rec.waitFor(t, "join 2/200")
	h.rec.waitFor(t, "leave 200")

	events := strings.Join(h.rec.all(), "\n")
	if strings.Index(events, "leave 100") > strings.Index(events, "join 2/200") {
		t.Errorf("guild 2 joined before guild 1 left:\n%s", events)
	}
}

func TestShutdownStopsPlaybackAndLeaves(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	h.m.Enqueue(1, track("one", 100, load("one", nil, nil)))
	h.m.Enqueue(1, track("two", 100, load("two", nil, nil)))
	h.rec.waitFor(t, "holding")
	h.cancel()
	close(hold)
	h.m.Wait()
	events := strings.Join(h.rec.all(), "\n")
	if !strings.Contains(events, "leave 100") || strings.Contains(events, "**two**") {
		t.Errorf("events after shutdown:\n%s", events)
	}
	if _, err := h.m.Enqueue(1, track("late", 100, load("late", nil, nil))); err == nil {
		t.Error("enqueue after shutdown should fail")
	}
}

func TestNameAndFormatting(t *testing.T) {
	if got := (Track{Title: "a*b_c `d` @everyone"}).Name(); got != "**a\\*b\\_c \\`d\\` @everyone**" {
		t.Errorf("Name = %q", got)
	}
	for d, want := range map[time.Duration]string{
		19 * time.Second:                          "0:19",
		3*time.Minute + 5*time.Second:             "3:05",
		time.Hour + 2*time.Minute + 3*time.Second: "1:02:03",
		1500 * time.Millisecond:                   "0:02",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestKickedFromVoiceDiscardsQueue(t *testing.T) {
	hold := make(chan struct{})
	h := newHarness(t, hold, 0)
	h.m.Enqueue(1, track("one", 100, load("one", nil, nil)))
	h.m.Enqueue(1, track("two", 100, load("two", nil, nil)))
	h.rec.waitFor(t, "holding")

	if !h.m.Disconnected(1) {
		t.Fatal("Disconnected should report a discarded queue")
	}
	close(hold)
	h.rec.waitFor(t, "msg 7: I was disconnected from voice, so I cleared the queue.")
	h.rec.waitFor(t, "leave 100")
	time.Sleep(20 * time.Millisecond)
	events := strings.Join(h.rec.all(), "\n")
	if strings.Contains(events, "**two**") || strings.Contains(events, "failed") {
		t.Errorf("after a kick, nothing else should play or report failure:\n%s", events)
	}
	if l := h.m.List(1); l.Current != nil || len(l.Upcoming) != 0 {
		t.Errorf("queue not discarded: %+v", l)
	}
	if h.m.Disconnected(1) {
		t.Error("a second disconnect event must be ignored")
	}

	// The server can queue again afterwards, with a fresh join.
	h.m.Enqueue(1, track("three", 100, load("three", nil, nil)))
	h.rec.waitFor(t, "join 1/100")
	h.rec.waitFor(t, "msg 7: Now playing: **three**")
}

func TestOwnLeavesAreNotKicks(t *testing.T) {
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	connector := &fakeConnector{rec: rec}
	m := New(ctx, Options{Connector: connector, Clock: instantClock{}, Notifier: fakeNotifier{rec: rec}})
	t.Cleanup(func() { cancel(); m.Wait() })
	// Discord reports every leave, including the bot's own; simulate that on each Close.
	connector.onClose = func() {
		if m.Disconnected(1) {
			rec.add("WRONGLY treated as kick")
		}
	}

	gate := make(chan struct{})
	m.Enqueue(1, track("one", 100, load("one", gate, nil)))
	m.Enqueue(1, track("two", 101, load("two", gate, nil))) // channel switch: leave 100, join 101
	close(gate)
	rec.waitFor(t, "msg 7: Now playing: **two**")
	rec.waitFor(t, "leave 101")
	time.Sleep(20 * time.Millisecond)
	if events := strings.Join(rec.all(), "\n"); strings.Contains(events, "WRONGLY") || strings.Contains(events, "disconnected") {
		t.Errorf("own leave treated as kick:\n%s", events)
	}
	if m.Disconnected(1) {
		t.Error("idle guild: Disconnected should be ignored")
	}
}
