package commands

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// DailyBalances stores each user's ask balance and the day it's for. store.DB
// implements it (SQLite); counts only, never content.
type DailyBalances interface {
	// DailyBalance returns user's stored balance; found is false if none.
	DailyBalance(ctx context.Context, user snowflake.ID) (day string, balance int, found bool, err error)
	SetDailyBalance(ctx context.Context, user snowflake.ID, day string, balance int) error
	// AllDailyBalances returns every stored balance (for the debug command).
	AllDailyBalances(ctx context.Context) ([]DailyRow, error)
}

// DailyRow is one stored balance.
type DailyRow struct {
	User    snowflake.ID
	Day     string
	Balance int
}

// Today returns everyone's balance today (with any reset applied), by user
// ID, and when the next reset is.
func (d *DailyLimit) Today(ctx context.Context) (rows []DailyRow, nextReset time.Time, err error) {
	if d == nil || d.Limit <= 0 {
		return nil, time.Time{}, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := d.Store.AllDailyBalances(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	today := d.day()
	for _, r := range all {
		if r.Day != today {
			r.Balance = d.Limit + min(r.Balance, 0)
			r.Day = today
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b DailyRow) int { return cmp.Compare(a.User, b.User) })
	loc := d.Location
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	y, m, day := now().In(loc).Date()
	return rows, time.Date(y, m, day+1, 0, 0, 0, 0, loc), nil
}

// DailyLimit is the per-user daily ask allowance, across all servers (the bot
// operator's choices):
//   - Each day starts at Limit. A question may go ahead while the balance is
//     above 0; afterwards its weight is taken off, so it can go below 0.
//   - Weight: a plain answer 1; one that searched the web WeightSearch, viewed
//     images WeightImage, both the sum. Only the provider knows, so answers are
//     charged after they arrive.
//   - At midnight in Location the balance resets to Limit plus any debt (−1 →
//     49 of 50). Unused balance never carries over.
//   - Owners are exempt unless LimitOwners.
type DailyLimit struct {
	Limit        int
	LimitOwners  bool
	IsOwner      func(snowflake.ID) bool
	Location     *time.Location // nil = UTC
	WeightSearch int
	WeightImage  int
	Store        DailyBalances
	Now          func() time.Time // nil = time.Now

	mu sync.Mutex // balance read-modify-write
}

// Weight is how much an answer with usage u costs.
func (d *DailyLimit) Weight(u llm.Usage) int {
	w := 0
	if u.WebSearch {
		w += d.WeightSearch
	}
	if u.ViewedImages {
		w += d.WeightImage
	}
	return max(w, 1)
}

// limits reports whether user is limited.
func (d *DailyLimit) limits(user snowflake.ID) bool { return !d.exempt(user) }

// exempt reports whether user isn't limited at all.
func (d *DailyLimit) exempt(user snowflake.ID) bool {
	return d == nil || d.Limit <= 0 || (!d.LimitOwners && d.IsOwner != nil && d.IsOwner(user))
}

// Balance returns user's balance today. ok is false when user isn't limited.
func (d *DailyLimit) Balance(ctx context.Context, user snowflake.ID) (balance int, ok bool, err error) {
	if d.exempt(user) {
		return 0, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := d.today(ctx, user)
	return b, true, err
}

// Charge takes an answer's weight off user's balance and returns what's left.
// ok is false when user isn't limited.
func (d *DailyLimit) Charge(ctx context.Context, user snowflake.ID, u llm.Usage) (balance int, ok bool, err error) {
	if d.exempt(user) {
		return 0, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := d.today(ctx, user)
	if err != nil {
		return 0, true, err
	}
	b -= d.Weight(u)
	return b, true, d.Store.SetDailyBalance(ctx, user, d.day(), b)
}

// today returns user's balance for today, applying a reset if the stored one
// is from an earlier day. Callers hold mu.
func (d *DailyLimit) today(ctx context.Context, user snowflake.ID) (int, error) {
	day, b, found, err := d.Store.DailyBalance(ctx, user)
	switch {
	case err != nil:
		return 0, err
	case !found:
		return d.Limit, nil
	case day != d.day():
		return d.Limit + min(b, 0), nil // debt carries over; savings don't
	}
	return b, nil
}

// day is today's date where the limit resets.
func (d *DailyLimit) day() string {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	loc := d.Location
	if loc == nil {
		loc = time.UTC
	}
	return now().In(loc).Format(time.DateOnly)
}
