package voice

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when Sleep is called (plus any injected work time).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// frames returns a next() that yields n numbered frames, then io.EOF.
func frames(n int) func() ([]byte, error) {
	i := 0
	return func() ([]byte, error) {
		if i == n {
			return nil, io.EOF
		}
		i++
		return []byte{byte(i)}, nil
	}
}

func TestPaceTwentyMillisecondGrid(t *testing.T) {
	clock := newFakeClock()
	start := clock.Now()
	var at []time.Duration
	n, err := Pace(context.Background(), clock, FrameDuration, frames(50), func([]byte) error {
		at = append(at, clock.Now().Sub(start))
		clock.advance(3 * time.Millisecond) // simulated send work must not accumulate as drift
		return nil
	})
	if err != nil || n != 50 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for i, got := range at {
		if want := time.Duration(i) * FrameDuration; got != want {
			t.Fatalf("frame %d written at %v, want %v", i, got, want)
		}
	}
}

func TestPaceKeepsOrder(t *testing.T) {
	var got []byte
	Pace(context.Background(), newFakeClock(), FrameDuration, frames(5), func(f []byte) error {
		got = append(got, f[0])
		return nil
	})
	if string(got) != "\x01\x02\x03\x04\x05" {
		t.Errorf("frames out of order: %v", got)
	}
}

func TestPaceResyncsAfterStall(t *testing.T) {
	clock := newFakeClock()
	start := clock.Now()
	var at []time.Duration
	Pace(context.Background(), clock, FrameDuration, frames(4), func([]byte) error {
		at = append(at, clock.Now().Sub(start))
		if len(at) == 2 {
			clock.advance(time.Second) // a long stall after frame 1
		}
		return nil
	})
	// Frame 2 goes out immediately after the stall; frame 3 follows 20 ms later,
	// instead of a burst of ~50 catch-up frames with no gaps.
	if at[2] != at[1]+time.Second || at[3] != at[2]+FrameDuration {
		t.Errorf("unexpected schedule after stall: %v", at)
	}
}

func TestPaceStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n, err := Pace(ctx, newFakeClock(), FrameDuration, frames(100), func([]byte) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Errorf("n=%d err=%v, want 1 frame then context.Canceled", n, err)
	}
}

func TestPaceStopsOnErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Pace(context.Background(), newFakeClock(), FrameDuration, func() ([]byte, error) { return nil, boom }, func([]byte) error { return nil }); !errors.Is(err, boom) {
		t.Errorf("source error: %v", err)
	}
	if n, err := Pace(context.Background(), newFakeClock(), FrameDuration, frames(10), func([]byte) error { return boom }); !errors.Is(err, boom) || n != 0 {
		t.Errorf("write error: n=%d err=%v", n, err)
	}
}

func TestRealClockSleepCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (RealClock{}).Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
