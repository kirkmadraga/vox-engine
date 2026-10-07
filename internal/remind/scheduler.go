package remind

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ErrGone is what a FireFunc returns when the reminder can't be delivered any
// more (its owner lost access, or the channel is gone): it's deleted.
var ErrGone = errors.New("reminder can't be delivered any more")

// FireFunc delivers r. late is true when it came due while the bot was off
// (before Run started), so it's being sent after its time; a reminder merely
// delayed behind others while the bot runs isn't late. Errors other than
// ErrGone are logged; the reminder then moves on as if sent (a one-off is
// deleted), so nothing piles up.
type FireFunc func(ctx context.Context, r Reminder, late bool) error

// DefaultLateLimit is how late a reminder may still be sent (after the bot
// was down); later, a one-off is dropped and a repeat skips to its next time.
const DefaultLateLimit = 24 * time.Hour

// maxWait caps one sleep, so a changed clock is noticed.
const maxWait = time.Hour

// Scheduler fires saved reminders when they're due, one at a time, soonest
// first. It reads the store before every step, so deletes and new reminders
// take effect without telling it; Wake makes it look again at once (call it
// after adding a reminder that may be due sooner).
type Scheduler struct {
	Store     Store
	Fire      FireFunc
	LateLimit time.Duration                        // 0 = DefaultLateLimit
	Now       func() time.Time                     // nil = time.Now
	After     func(time.Duration) <-chan time.Time // nil = time.After
	Logger    *slog.Logger                         // nil = discard

	once    sync.Once
	wake    chan struct{}
	started time.Time // when Run began: reminders due before it are late

	// unrecorded are reminders sent (or found gone) whose delete or
	// reschedule failed, by ID, with the due time they had. Only Run's
	// goroutine uses it. While the store can't write, they're retried
	// without being sent again: a full disk mustn't ping someone every minute.
	unrecorded map[int64]unrecorded

	fired, late, dropped, gone, failed atomic.Int64
	nextDue                            atomic.Int64 // Unix seconds; 0 = none
}

// Stats are the scheduler's counts since start, for the owner's debug command.
type Stats struct {
	Fired, Late, Dropped, Gone, Failed int64
	NextDue                            time.Time // zero if none
}

// Stats returns the counts since start.
func (s *Scheduler) Stats() Stats {
	st := Stats{Fired: s.fired.Load(), Late: s.late.Load(), Dropped: s.dropped.Load(), Gone: s.gone.Load(), Failed: s.failed.Load()}
	if n := s.nextDue.Load(); n != 0 {
		st.NextDue = time.Unix(n, 0)
	}
	return st
}

// unrecorded is a handled reminder the store didn't record yet.
type unrecorded struct {
	next time.Time // its due time when handled
	gone bool      // delete it (rather than move it on)
}

func (s *Scheduler) init() {
	s.wake = make(chan struct{}, 1)
	s.unrecorded = map[int64]unrecorded{}
}

// Wake makes a running scheduler look at the store again now.
func (s *Scheduler) Wake() {
	s.once.Do(s.init)
	select {
	case s.wake <- struct{}{}:
	default: // already pending
	}
}

// Run fires reminders until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	s.once.Do(s.init)
	s.started = s.now()
	for {
		wait, err := s.step(ctx)
		if err != nil {
			s.logger().Error("remind: couldn't read reminders", "err", err)
			wait = time.Minute
		}
		if wait == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-s.after(wait):
		}
	}
}

// step handles the soonest reminder if it's due, and returns how long to wait
// before the next look (0: look again at once).
func (s *Scheduler) step(ctx context.Context) (time.Duration, error) {
	s.once.Do(s.init)
	r, ok, err := s.Store.SoonestReminder(ctx)
	if err != nil {
		return 0, err
	}
	if !ok {
		s.nextDue.Store(0)
		return maxWait, nil
	}
	s.nextDue.Store(r.Next.Unix())
	now := s.now()
	if wait := r.Next.Sub(now); wait > 0 {
		return min(wait, maxWait), nil
	}
	if u, ok := s.unrecorded[r.ID]; ok && u.next.Equal(r.Next) {
		// Already sent: only record it this time.
		if err := s.record(ctx, r, now, u.gone); err != nil {
			return 0, err // still can't: Run waits a minute
		}
		delete(s.unrecorded, r.ID)
		return 0, nil
	}
	if err := s.handle(ctx, r, now); err != nil {
		// It couldn't be deleted or moved on: wait (Run's minute), and next
		// time only record it, never send it again.
		return 0, err
	}
	if ctx.Err() != nil {
		return maxWait, nil // shutting down; Run returns
	}
	return 0, nil
}

// handle sends r (or skips it if too late) and moves it on. An error means
// the store couldn't record that.
func (s *Scheduler) handle(ctx context.Context, r Reminder, now time.Time) error {
	log := s.logger().With("reminder", r.ID, "user", r.UserID, "guild", r.GuildID, "channel", r.ChannelID)
	overdue := now.Sub(r.Next)
	limit := s.LateLimit
	if limit <= 0 {
		limit = DefaultLateLimit
	}
	if overdue > limit {
		s.dropped.Add(1)
		log.Warn("remind: too late to send; skipped", "overdue", overdue.Round(time.Second), "repeats", !r.Rule.Once())
		return s.moveOn(ctx, r, now)
	}
	late := r.Next.Before(s.started)
	err := s.Fire(ctx, r, late)
	gone := false
	switch {
	case ctx.Err() != nil:
		return nil // shutting down: untouched, so it's sent (late) after a restart
	case errors.Is(err, ErrGone):
		gone = true
		s.gone.Add(1)
		log.Info("remind: can't be delivered any more; deleted", "err", err)
	case err != nil:
		s.failed.Add(1)
		log.Error("remind: couldn't send", "err", err)
	default:
		s.fired.Add(1)
		if late {
			s.late.Add(1)
		}
		log.Info("remind: sent", "late", late, "repeats", !r.Rule.Once())
	}
	if err := s.record(ctx, r, now, gone); err != nil {
		s.unrecorded[r.ID] = unrecorded{next: r.Next, gone: gone}
		return err
	}
	return nil
}

// record deletes a handled reminder that's gone or a one-off, and moves a
// repeat to its next time after now.
func (s *Scheduler) record(ctx context.Context, r Reminder, now time.Time, gone bool) error {
	if gone || r.Rule.Once() {
		_, err := s.Store.DeleteReminder(ctx, r.ID)
		return err
	}
	return s.Store.RescheduleReminder(ctx, r.ID, r.Rule.Next(r.Next, now, r.location()))
}

// moveOn records a reminder that was skipped, not sent.
func (s *Scheduler) moveOn(ctx context.Context, r Reminder, now time.Time) error {
	return s.record(ctx, r, now, false)
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) after(d time.Duration) <-chan time.Time {
	if s.After != nil {
		return s.After(d)
	}
	return time.After(d)
}

func (s *Scheduler) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}
