package commands

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// The daily ask allowance: the bot operator's rules (see DailyLimit).

type memBalances struct {
	mu   sync.Mutex
	rows map[snowflake.ID]struct {
		day string
		b   int
	}
	fail bool
}

func (m *memBalances) DailyBalance(_ context.Context, user snowflake.ID) (string, int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return "", 0, false, errors.New("storage down")
	}
	r, ok := m.rows[user]
	return r.day, r.b, ok, nil
}

func (m *memBalances) AllDailyBalances(context.Context) ([]DailyRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DailyRow
	for u, r := range m.rows {
		out = append(out, DailyRow{User: u, Day: r.day, Balance: r.b})
	}
	return out, nil
}

func (m *memBalances) SetDailyBalance(_ context.Context, user snowflake.ID, day string, b int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[snowflake.ID]struct {
			day string
			b   int
		}{}
	}
	m.rows[user] = struct {
		day string
		b   int
	}{day, b}
	return nil
}

var zone8 = time.FixedZone("UTC+8", 8*60*60) // a reset timezone other than UTC

// newDaily starts at 2026-09-30 12:00 in zone8.
func newDaily(limit int) (*DailyLimit, *clock) {
	clk := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, zone8)}
	return &DailyLimit{
		Limit: limit, WeightSearch: 2, WeightImage: 4, Location: zone8, Now: clk.now,
		Store:   &memBalances{},
		IsOwner: func(id snowflake.ID) bool { return id == askOwner },
	}, clk
}

func balance(t *testing.T, d *DailyLimit, user snowflake.ID) int {
	t.Helper()
	b, ok, err := d.Balance(context.Background(), user)
	if err != nil || !ok {
		t.Fatalf("Balance: %v, limited=%v", err, ok)
	}
	return b
}

func charge(t *testing.T, d *DailyLimit, user snowflake.ID, u llm.Usage) int {
	t.Helper()
	b, _, err := d.Charge(context.Background(), user, u)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDailyWeights(t *testing.T) {
	d, _ := newDaily(50)
	cases := map[llm.Usage]int{
		{}:                                    1,
		{WebSearch: true}:                     2,
		{ViewedImages: true}:                  4,
		{WebSearch: true, ViewedImages: true}: 6, // they add up
	}
	for u, want := range cases {
		if got := d.Weight(u); got != want {
			t.Errorf("Weight(%+v) = %d, want %d", u, got, want)
		}
	}
	var off *DailyLimit // no limit configured: like its other methods, nil is safe
	if got := off.Weight(llm.Usage{WebSearch: true}); got != 1 {
		t.Errorf("nil Weight = %d, want 1", got)
	}
}

func TestDailyBalanceStartsFullAndGoesIntoDebt(t *testing.T) {
	d, _ := newDaily(5)
	if b := balance(t, d, 8); b != 5 {
		t.Fatalf("a new user starts with %d, want 5", b)
	}
	charge(t, d, 8, llm.Usage{})                        // 4
	charge(t, d, 8, llm.Usage{WebSearch: true})         // 2
	b := charge(t, d, 8, llm.Usage{ViewedImages: true}) // -2: the last question may overshoot
	if b != -2 {
		t.Errorf("balance %d, want -2", b)
	}
	if other := balance(t, d, 9); other != 5 {
		t.Errorf("another user's balance changed: %d", other)
	}
}

// At midnight in the reset timezone, debt carries over and savings don't.
func TestDailyResetCarriesDebtNotSavings(t *testing.T) {
	d, clk := newDaily(50)
	for range 3 {
		charge(t, d, 8, llm.Usage{}) // 47 left
	}
	for range 13 {
		charge(t, d, 9, llm.Usage{ViewedImages: true}) // 50 - 52 = -2
	}
	clk.advance(11*time.Hour + 59*time.Minute) // 23:59 there: still today
	if b := balance(t, d, 9); b != -2 {
		t.Fatalf("before midnight: %d", b)
	}
	clk.advance(time.Minute) // 00:00 there (16:00 UTC)
	if b := balance(t, d, 8); b != 50 {
		t.Errorf("unused balance must not carry over: %d, want 50", b)
	}
	if b := balance(t, d, 9); b != 48 {
		t.Errorf("debt must carry over: %d, want 48", b)
	}
	// A skipped day doesn't erase the debt either.
	d2, clk2 := newDaily(50)
	for range 13 {
		charge(t, d2, 9, llm.Usage{ViewedImages: true})
	}
	clk2.advance(72 * time.Hour)
	if b := balance(t, d2, 9); b != 48 {
		t.Errorf("after 3 days: %d, want 48", b)
	}
}

func TestDailyOwnersExemptUnlessOptedIn(t *testing.T) {
	d, _ := newDaily(1)
	if _, limited, _ := d.Balance(context.Background(), askOwner); limited {
		t.Error("owners are exempt by default")
	}
	d.LimitOwners = true
	if _, limited, _ := d.Balance(context.Background(), askOwner); !limited {
		t.Error("with LimitOwners, owners are limited too")
	}
	var none *DailyLimit
	if _, limited, _ := none.Balance(context.Background(), 8); limited {
		t.Error("no daily limit configured: nobody is limited")
	}
	d0, _ := newDaily(0)
	if _, limited, _ := d0.Balance(context.Background(), 8); limited {
		t.Error("limit 0 means no limit")
	}
}
