package voice

import (
	"context"
	"errors"
	"io"
	"time"
)

// Clock abstracts time so the pacer can be tested without real sleeps.
type Clock interface {
	Now() time.Time
	// Sleep waits for d, returning ctx.Err() early if ctx is cancelled.
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxLag is how far behind schedule the pacer may fall before it stops trying
// to catch up and restarts its schedule from now (e.g. after a system stall).
const maxLag = 3 * FrameDuration

// Pace writes frames from next on a fixed grid (start, start+interval, ...).
// Anchoring to the start time, rather than sleeping a fixed interval after each
// write, keeps playback from drifting. It stops at io.EOF from next (returning
// the number of frames written and nil), on the first error, or when ctx is done.
func Pace(ctx context.Context, clock Clock, interval time.Duration, next func() ([]byte, error), write func([]byte) error) (int, error) {
	start := clock.Now()
	for i := 0; ; i++ {
		frame, err := next()
		if errors.Is(err, io.EOF) {
			return i, nil
		}
		if err != nil {
			return i, err
		}

		due := start.Add(time.Duration(i) * interval)
		if wait := due.Sub(clock.Now()); wait > 0 {
			if err := clock.Sleep(ctx, wait); err != nil {
				return i, err
			}
		} else if -wait > maxLag {
			start = clock.Now().Add(-time.Duration(i) * interval)
		}
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if err := write(frame); err != nil {
			return i, err
		}
	}
}
