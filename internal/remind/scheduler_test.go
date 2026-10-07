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

func (m *memStore) deleteWhere(f func(Reminder) bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.rs)
	m.rs = slices.DeleteFunc(m.rs, f)
	return n - len(m.rs)
}

func (m *memStore) DeleteUserReminders(_ context.Context, user snowflake.ID) (int, error) {
	return m.deleteWhere(func(r Reminder) bool { return r.UserID == user }), nil
}

func (m *memStore) DeleteGuildReminders(_ context.Context, guild snowflake.ID, keep []snowflake.ID) (int, error) {
	return m.deleteWhere(func(r Reminder) bool { return r.GuildID == guild && !slices.Contains(keep, r.UserID) }), nil
}

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
	err   error // what Fire returns
}

func newHarness() *harness {
	h := &harness{store: &memStore{}, clock: now}
	h.s = &Scheduler{Store: h.store, Now: func() time.Time { return h.clock },
		Fire: func(_ context.Context, r Reminder, late bool) error {
			h.fired = append(h.fired, firing{r.ID, late})
			return h.err
		}}
	return h
}

func (h *harness) add(next time.Time, rule Rule) int64 {
	id, _ := h.store.AddReminder(context.Background(), Reminder{UserID: 8, GuildID: 100, ChannelID: 5, Text: "x", Rule: rule, Location: "UTC", Next: next})
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
	h.clock = now.Add(3 * time.Hour)
	h.runUntilIdle(t)
	if !slices.Equal(h.fired, []firing{{a, false}, {b, true}}) || len(h.store.all()) != 0 {
		t.Errorf("fired %v, left %+v", h.fired, h.store.all())
	}
	if st := h.s.Stats(); st.Fired != 2 || st.Late != 1 || !st.NextDue.IsZero() {
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

// If the store can't record that a reminder was sent, the scheduler backs
// off instead of sending it again at once.
func TestSchedulerBacksOffWhenTheStoreFails(t *testing.T) {
	h := newHarness()
	h.add(now, Rule{})
	h.store.fail = errors.New("disk full")
	if _, err := h.s.step(context.Background()); err == nil || len(h.fired) != 1 {
		t.Fatalf("want an error after one send, got %v (fired %v)", err, h.fired)
	}
}

// A shutdown during a send leaves the reminder for after the restart.
func TestSchedulerLeavesItOnShutdown(t *testing.T) {
	h := newHarness()
	h.add(now, Rule{})
	ctx, cancel := context.WithCancel(context.Background())
	h.s.Fire = func(context.Context, Reminder, bool) error { cancel(); return context.Canceled }
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
	h.s.Fire = func(_ context.Context, r Reminder, _ bool) error { fired <- r.ID; return nil }
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
