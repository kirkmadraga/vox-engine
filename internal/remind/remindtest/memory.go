// Package remindtest has an in-memory remind.Store for tests.
package remindtest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/remind"
)

// Memory is an in-memory remind.Store. Fail, when set, is returned by every
// method.
type Memory struct {
	mu     sync.Mutex
	rs     []remind.Reminder
	nextID int64
	Fail   error
}

var _ remind.Store = (*Memory)(nil)

// All returns every reminder, soonest first.
func (m *Memory) All() []remind.Reminder {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted()
}

func (m *Memory) sorted() []remind.Reminder {
	out := slices.Clone(m.rs)
	slices.SortStableFunc(out, func(a, b remind.Reminder) int { return a.Next.Compare(b.Next) })
	return out
}

func (m *Memory) AddReminder(_ context.Context, r remind.Reminder) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	m.nextID++
	r.ID = m.nextID
	m.rs = append(m.rs, r)
	return r.ID, nil
}

func (m *Memory) UserReminders(_ context.Context, user snowflake.ID) ([]remind.Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []remind.Reminder
	for _, r := range m.sorted() {
		if r.UserID == user {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Memory) SoonestReminder(context.Context) (remind.Reminder, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil || len(m.rs) == 0 {
		return remind.Reminder{}, false, m.Fail
	}
	return m.sorted()[0], true, nil
}

func (m *Memory) RescheduleReminder(_ context.Context, id int64, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for i := range m.rs {
		if m.rs[i].ID == id {
			m.rs[i].Next = next
		}
	}
	return nil
}

func (m *Memory) DeleteReminder(_ context.Context, id int64) (bool, error) {
	n, err := m.deleteWhere(func(r remind.Reminder) bool { return r.ID == id })
	return n > 0, err
}

func (m *Memory) DeleteUserReminders(_ context.Context, user snowflake.ID) (int, error) {
	return m.deleteWhere(func(r remind.Reminder) bool { return r.UserID == user })
}

func (m *Memory) DeleteGuildReminders(_ context.Context, guild snowflake.ID, keep []snowflake.ID) (int, error) {
	return m.deleteWhere(func(r remind.Reminder) bool { return r.GuildID == guild && !slices.Contains(keep, r.UserID) })
}

func (m *Memory) deleteWhere(f func(remind.Reminder) bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	n := len(m.rs)
	m.rs = slices.DeleteFunc(m.rs, f)
	return n - len(m.rs), nil
}

func (m *Memory) ReminderCounts(context.Context) (total, repeating int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rs {
		if !r.Rule.Once() {
			repeating++
		}
	}
	return len(m.rs), repeating, m.Fail
}
