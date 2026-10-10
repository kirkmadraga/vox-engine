package remind

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// memStore is an in-memory Store.
type memStore struct {
	mu     sync.Mutex
	rs     []Reminder
	nextID int64
	fail   error // returned by deletes and reschedules when set
}

func (m *memStore) AddReminder(_ context.Context, r Reminder) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	r.ID = m.nextID
	m.rs = append(m.rs, r)
	return r.ID, nil
}

func (m *memStore) sorted() []Reminder {
	out := slices.Clone(m.rs)
	slices.SortStableFunc(out, func(a, b Reminder) int { return a.Next.Compare(b.Next) })
	return out
}

func (m *memStore) UserReminders(_ context.Context, user snowflake.ID) ([]Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Reminder
	for _, r := range m.sorted() {
		if r.UserID == user {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) SoonestReminder(context.Context) (Reminder, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rs) == 0 {
		return Reminder{}, false, nil
	}
	return m.sorted()[0], true, nil
}

func (m *memStore) RescheduleReminder(_ context.Context, id int64, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	for i := range m.rs {
		if m.rs[i].ID == id {
			m.rs[i].Next = next
		}
	}
	return nil
}

func (m *memStore) DeleteReminder(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	n := len(m.rs)
	m.rs = slices.DeleteFunc(m.rs, func(r Reminder) bool { return r.ID == id })
	return len(m.rs) < n, nil
}

func (m *memStore) AllReminders(context.Context) ([]Reminder, error) { return m.all(), nil }

func (m *memStore) ReminderCounts(context.Context) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep := 0
	for _, r := range m.rs {
		if !r.Rule.Once() {
			rep++
		}
	}
	return len(m.rs), rep, nil
}

func (m *memStore) all() []Reminder {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted()
}

var _ Store = (*memStore)(nil)

type firing struct {
	id   int64
	late bool
}

// harness: a scheduler over memStore with a settable clock and a recording Fire.
type harness struct {
	s     *Scheduler
	store *memStore
	clock time.Time
	fired []firing
	last  []bool // each firing's Last, in order
	err   error  // what Fire returns
}

func newHarness() *harness {
	h := &harness{store: &memStore{}, clock: now}
	h.s = &Scheduler{Store: h.store, Now: func() time.Time { return h.clock },
		Fire: func(_ context.Context, r Reminder, f Firing) error {
			h.fired = append(h.fired, firing{r.ID, f.Late})
			h.last = append(h.last, f.Last)
			return h.err
		}}
	return h
}

func (h *harness) add(next time.Time, rule Rule) int64 {
	return h.addUntil(next, rule, time.Time{})
}

func (h *harness) addUntil(next time.Time, rule Rule, until time.Time) int64 {
	id, _ := h.store.AddReminder(context.Background(), Reminder{UserID: 8, GuildID: 100, ChannelID: 5, Text: "x", Rule: rule,
		Location: "Asia/Singapore", Next: next, Until: until}) // UTC+8, like now
	return id
}

// runUntilIdle steps until nothing is due, and returns the last wait.
func (h *harness) runUntilIdle(t *testing.T) time.Duration {
	t.Helper()
	for range 100 {
		wait, err := h.s.step(context.Background())
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		if wait > 0 {
			return wait
		}
	}
	t.Fatal("never idle")
	return 0
}

func TestSchedulerFiresWhenDueAndDeletesOneOffs(t *testing.T) {
	h := newHarness()
	a := h.add(now.Add(time.Hour), Rule{})
	b := h.add(now.Add(2*time.Hour), Rule{})
	if wait := h.runUntilIdle(t); wait != time.Hour || len(h.fired) != 0 {
		t.Fatalf("before: wait %v, fired %v", wait, h.fired)
	}
	h.clock = now.Add(time.Hour)
	if wait := h.runUntilIdle(t); wait != time.Hour || !slices.Equal(h.fired, []firing{{a, false}}) {
		t.Fatalf("at the first: wait %v, fired %v", wait, h.fired)
	}
	// Handled an hour after its time while the bot runs (e.g. behind others):
	// delayed, not "late" (the bot wasn't off).
	h.clock = now.Add(3 * time.Hour)
	h.runUntilIdle(t)
	if !slices.Equal(h.fired, []firing{{a, false}, {b, false}}) || len(h.store.all()) != 0 {
		t.Errorf("fired %v, left %+v", h.fired, h.store.all())
	}
	if st := h.s.Stats(); st.Fired != 2 || st.Late != 0 || !st.NextDue.IsZero() {
		t.Errorf("stats %+v", st)
	}
}

// "Late" means it came due while the bot was off: before this run started.
func TestSchedulerLateMeansDueWhileOff(t *testing.T) {
	h := newHarness()
	before := h.add(now.Add(-time.Hour), Rule{})
	h.add(now.Add(time.Minute), Rule{})
	h.s.started = now // the bot came back at now
	h.runUntilIdle(t)
	h.clock = now.Add(10 * time.Minute) // the second is handled 9 minutes after its time
	h.runUntilIdle(t)
	if len(h.fired) != 2 || h.fired[0] != (firing{before, true}) || h.fired[1].late {
		t.Errorf("fired %v; want only the one due while off marked late", h.fired)
	}
	if st := h.s.Stats(); st.Late != 1 {
		t.Errorf("stats %+v", st)
	}
}

func TestSchedulerRepeats(t *testing.T) {
	h := newHarness()
	id := h.add(now, Rule{Every: 2 * time.Hour})
	h.runUntilIdle(t)
	if rs := h.store.all(); len(rs) != 1 || !rs[0].Next.Equal(now.Add(2*time.Hour)) || len(h.fired) != 1 {
		t.Fatalf("after firing: %+v, fired %v", rs, h.fired)
	}
	// Down for 5 hours: sent once (late), then back on its rhythm; no burst.
	h.clock = now.Add(7 * time.Hour)
	h.s.started = h.clock // restarted
	h.runUntilIdle(t)
	if rs := h.store.all(); !rs[0].Next.Equal(now.Add(8*time.Hour)) || !slices.Equal(h.fired, []firing{{id, false}, {id, true}}) {
		t.Errorf("after a gap: next %v, fired %v", rs[0].Next, h.fired)
	}
}

func TestSchedulerSkipsWhatsTooLate(t *testing.T) {
	h := newHarness()
	h.add(now, Rule{})
	rep := h.add(now, Rule{Days: [7]bool{true, true, true, true, true, true, true}, Hour: 9})
	h.clock = now.Add(DefaultLateLimit + time.Minute)
	h.runUntilIdle(t)
	rs := h.store.all()
	if len(h.fired) != 0 || len(rs) != 1 || rs[0].ID != rep || !rs[0].Next.After(h.clock) {
		t.Errorf("fired %v, left %+v", h.fired, rs)
	}
	if st := h.s.Stats(); st.Dropped != 2 {
		t.Errorf("stats %+v", st)
	}
}

func TestSchedulerDeletesWhatsGoneAndMovesOnAfterFailures(t *testing.T) {
	h := newHarness()
	h.add(now, Rule{Every: time.Hour})
	h.err = ErrGone
	h.runUntilIdle(t)
	if len(h.store.all()) != 0 {
		t.Errorf("a gone reminder must be deleted, even a repeat: %+v", h.store.all())
	}
	h.add(now, Rule{Every: time.Hour})
	h.err = errors.New("discord down")
	h.runUntilIdle(t)
	if rs := h.store.all(); len(rs) != 1 || !rs[0].Next.Equal(now.Add(time.Hour)) || len(h.fired) != 2 {
		t.Errorf("a failed send moves on (no retry storm): %+v, fired %v", rs, h.fired)
	}
	if st := h.s.Stats(); st.Gone != 1 || st.Failed != 1 {
		t.Errorf("stats %+v", st)
	}
}

// If the store can read but not write (a full disk), a due reminder is sent
// once: later steps only retry recording it, never send it again.
func TestSchedulerSendsOnceWhileTheStoreCantWrite(t *testing.T) {
	for _, c := range []struct {
		name string
		rule Rule
		err  error
	}{
		{"one-off", Rule{}, nil},
		{"repeat", Rule{Every: time.Hour}, nil},
		{"gone", Rule{Every: time.Hour}, ErrGone},
	} {
		h := newHarness()
		id := h.add(now, c.rule)
		h.err = c.err
		h.store.fail = errors.New("disk full")
		for i := range 5 {
			if _, err := h.s.step(context.Background()); err == nil {
				t.Fatalf("%s, step %d: want the store's error", c.name, i)
			}
			h.clock = h.clock.Add(time.Minute) // Run's back-off
		}
		if len(h.fired) != 1 {
			t.Errorf("%s: sent %d times while the store couldn't write, want 1", c.name, len(h.fired))
		}
		h.store.fail = nil // the disk has room again
		h.runUntilIdle(t)
		rs := h.store.all()
		switch {
		case len(h.fired) != 1:
			t.Errorf("%s: sent again once recorded: %v", c.name, h.fired)
		case c.rule.Once() || c.err != nil:
			if len(rs) != 0 {
				t.Errorf("%s: want it deleted, left %+v", c.name, rs)
			}
		case len(rs) != 1 || rs[0].ID != id || !rs[0].Next.After(h.clock):
			t.Errorf("%s: want it moved on, got %+v", c.name, rs)
		}
	}
}

// A repeat with an end fires up to and including it, marks the last one,
// then is deleted.
func TestSchedulerStopsAtTheEnd(t *testing.T) {
	h := newHarness()
	id := h.addUntil(now, Rule{Every: 2 * time.Hour}, now.Add(4*time.Hour))
	for i := range 3 {
		h.clock = now.Add(time.Duration(i) * 2 * time.Hour)
		h.runUntilIdle(t)
	}
	if !slices.Equal(h.fired, []firing{{id, false}, {id, false}, {id, false}}) || !slices.Equal(h.last, []bool{false, false, true}) {
		t.Errorf("fired %v, last %v; want three, the third marked last", h.fired, h.last)
	}
	if rs := h.store.all(); len(rs) != 0 {
		t.Errorf("left %+v; want it deleted after the end", rs)
	}
	// Without an end, nothing is ever marked last.
	h = newHarness()
	h.add(now, Rule{Every: 2 * time.Hour})
	h.runUntilIdle(t)
	if len(h.store.all()) != 1 || !slices.Equal(h.last, []bool{false}) {
		t.Errorf("no end: last %v, left %+v", h.last, h.store.all())
	}
	// A one-off isn't "the last one" either.
	h = newHarness()
	h.add(now, Rule{})
	h.runUntilIdle(t)
	if !slices.Equal(h.last, []bool{false}) {
		t.Errorf("one-off: last %v", h.last)
	}
}

// Offline until after the end: what was missed is dropped, not sent late
// (the author's call), and the reminder is gone.
func TestSchedulerEndWhileOffline(t *testing.T) {
	h := newHarness()
	h.addUntil(now, Rule{Every: 2 * time.Hour}, now.Add(2*time.Hour))
	h.clock = now.Add(5 * time.Hour)
	h.s.started = h.clock
	h.runUntilIdle(t)
	if len(h.fired) != 0 || len(h.store.all()) != 0 {
		t.Errorf("fired %v left %+v; want nothing sent, and it gone", h.fired, h.store.all())
	}
	if st := h.s.Stats(); st.Dropped != 1 {
		t.Errorf("stats %+v; want it counted as dropped", st)
	}
	// Back exactly at its end (not past it): the missed one still goes, late, as the last.
	h = newHarness()
	id := h.addUntil(now, Rule{Every: 2 * time.Hour}, now.Add(2*time.Hour))
	h.clock = now.Add(2 * time.Hour)
	h.s.started = h.clock
	h.runUntilIdle(t)
	if !slices.Equal(h.fired, []firing{{id, true}}) || !slices.Equal(h.last, []bool{true}) || len(h.store.all()) != 0 {
		t.Errorf("back at the end: fired %v last %v left %+v; want one late last send", h.fired, h.last, h.store.all())
	}
	// Running, but handled a moment after the end (e.g. behind others): not
	// missed, so the last one still goes.
	h = newHarness()
	h.s.started = now
	id = h.addUntil(now.Add(2*time.Hour), Rule{Every: 2 * time.Hour}, now.Add(2*time.Hour))
	h.clock = now.Add(2*time.Hour + 3*time.Second)
	h.runUntilIdle(t)
	if !slices.Equal(h.fired, []firing{{id, false}}) || !slices.Equal(h.last, []bool{true}) {
		t.Errorf("a moment late while running: fired %v last %v; want it sent as the last", h.fired, h.last)
	}
	// Offline partway: sent late, then back on its rhythm, still ending on time.
	h = newHarness()
	id = h.addUntil(now, Rule{Every: 2 * time.Hour}, now.Add(8*time.Hour))
	h.clock = now.Add(3 * time.Hour)
	h.s.started = h.clock
	h.runUntilIdle(t)
	if rs := h.store.all(); len(rs) != 1 || !rs[0].Next.Equal(now.Add(4*time.Hour)) || !slices.Equal(h.last, []bool{false}) {
		t.Fatalf("partway: left %+v, last %v", rs, h.last)
	}
	for _, at := range []time.Duration{4, 6, 8, 10} {
		h.clock = now.Add(at * time.Hour)
		h.runUntilIdle(t)
	}
	if len(h.fired) != 4 || !slices.Equal(h.last, []bool{false, false, false, true}) || len(h.store.all()) != 0 {
		t.Errorf("partway: fired %v last %v left %+v; want 4 (the 8h one last), then gone", h.fired, h.last, h.store.all())
	}
}

// Too late to send and past its end: deleted without a send.
func TestSchedulerTooLateAndPastTheEnd(t *testing.T) {
	h := newHarness()
	h.addUntil(now, Rule{Every: 2 * time.Hour}, now.Add(4*time.Hour))
	keep := h.addUntil(now, Rule{Every: 2 * time.Hour}, now.Add(DefaultLateLimit+10*time.Hour))
	h.clock = now.Add(DefaultLateLimit + time.Minute)
	h.runUntilIdle(t)
	if rs := h.store.all(); len(h.fired) != 0 || len(rs) != 1 || rs[0].ID != keep {
		t.Errorf("fired %v left %+v; want the ended one deleted unsent, the other skipped ahead", h.fired, rs)
	}
}

// A day-based repeat ends on the right local day: one ending at the end of
// Friday includes Friday's; one ending before Friday's time doesn't.
func TestSchedulerDayRuleEndsOnTheLocalDay(t *testing.T) {
	daily := Rule{Days: [7]bool{true, true, true, true, true, true, true}, Hour: 8}
	thu, fri := at(2026, 10, 8, 8, 0), at(2026, 10, 9, 8, 0)
	for _, c := range []struct {
		until time.Time
		fires int
	}{
		{endOf(2026, 10, 9), 2},      // through Friday
		{at(2026, 10, 9, 8, 0), 2},   // exactly Friday 08:00: included
		{at(2026, 10, 9, 7, 59), 1},  // just before: Thursday is the last
		{at(2026, 10, 8, 23, 59), 1}, // late Thursday
		{at(2026, 10, 10, 0, 0), 2},  // midnight after Friday
		{at(2026, 10, 10, 8, 0), 3},  // Saturday's too
	} {
		h := newHarness()
		h.addUntil(thu, daily, c.until)
		for _, day := range []time.Time{thu, fri, fri.AddDate(0, 0, 1), fri.AddDate(0, 0, 2)} {
			h.clock = day
			h.runUntilIdle(t)
		}
		want := make([]bool, c.fires)
		want[c.fires-1] = true
		if !slices.Equal(h.last, want) || len(h.store.all()) != 0 {
			t.Errorf("until %v: last %v left %d; want %d sends, the final one last", c.until, h.last, len(h.store.all()), c.fires)
		}
	}
}

// A shutdown during a send leaves the reminder for after the restart.
func TestSchedulerLeavesItOnShutdown(t *testing.T) {
	h := newHarness()
	h.add(now, Rule{})
	ctx, cancel := context.WithCancel(context.Background())
	h.s.Fire = func(context.Context, Reminder, Firing) error { cancel(); return context.Canceled }
	h.s.step(ctx)
	if len(h.store.all()) != 1 {
		t.Error("the reminder was dropped on shutdown")
	}
}

// Run sleeps until the soonest is due, and Wake cuts a sleep short.
func TestSchedulerRunAndWake(t *testing.T) {
	h := newHarness()
	ticks := make(chan time.Time)
	waits := make(chan time.Duration, 10)
	h.s.After = func(d time.Duration) <-chan time.Time { waits <- d; return ticks }
	fired := make(chan int64, 10)
	h.s.Fire = func(_ context.Context, r Reminder, _ Firing) error { fired <- r.ID; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()

	if w := <-waits; w != maxWait {
		t.Errorf("nothing saved: waits %v", w)
	}
	id := h.add(now.Add(10*time.Minute), Rule{})
	h.s.Wake()
	if w := <-waits; w != 10*time.Minute {
		t.Errorf("after Wake: waits %v, want 10m", w)
	}
	h.clock = now.Add(10 * time.Minute) // only Run reads the clock, after the tick
	ticks <- time.Time{}
	if got := <-fired; got != id {
		t.Errorf("fired %d", got)
	}
	<-waits
	cancel()
	<-done
}
