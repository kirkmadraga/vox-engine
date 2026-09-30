package ytdlp

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// stageTimer records when a download's stage markers appear on yt-dlp's
// stdout, to log where the time goes:
//
//	extract  start → VOXMETA (metadata, incl. solving YouTube's JS challenge)
//	download VOXSTAGE download → VOXSTAGE convert (transferring the audio)
//	convert  VOXSTAGE convert → VOXFILE (ffmpeg rewrapping WebM as Ogg)
//	total    the whole yt-dlp run
//
// A stage whose markers didn't appear (runner without line output, a failure,
// an older yt-dlp) is left out; total is always logged.
type stageTimer struct {
	now func() time.Time

	mu                   sync.Mutex
	start, stop          time.Time
	meta, dl, conv, file time.Time
}

func newStageTimer(now func() time.Time) *stageTimer {
	if now == nil {
		now = time.Now
	}
	return &stageTimer{now: now}
}

func (t *stageTimer) begin() { t.mu.Lock(); t.start = t.now(); t.mu.Unlock() }
func (t *stageTimer) end()   { t.mu.Lock(); t.stop = t.now(); t.mu.Unlock() }

// line notes the first time each marker appears.
func (t *stageTimer) line(l string) {
	l = strings.TrimSpace(l)
	t.mu.Lock()
	defer t.mu.Unlock()
	mark := func(at *time.Time) {
		if at.IsZero() {
			*at = t.now()
		}
	}
	switch {
	case strings.HasPrefix(l, metaMarker):
		mark(&t.meta)
	case l == stageMarker+"download":
		mark(&t.dl)
	case l == stageMarker+"convert":
		mark(&t.conv)
	case strings.HasPrefix(l, fileMarker):
		mark(&t.file)
	}
}

// attrs are the log attributes for video id's run, which ended with err.
func (t *stageTimer) attrs(id string, err error) []any {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := []any{"video", id}
	span := func(name string, from, to time.Time) {
		if !from.IsZero() && !to.IsZero() && !to.Before(from) {
			a = append(a, name, round(to.Sub(from)))
		}
	}
	span("extract", t.start, t.meta)
	span("download", t.dl, t.conv)
	span("convert", t.conv, t.file)
	span("total", t.start, t.stop)
	return append(a, "outcome", outcome(err))
}

// outcome names how a run ended, for logs.
func outcome(err error) string {
	var yerr *Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &yerr):
		return yerr.Kind.String()
	default:
		return "failed"
	}
}

func round(d time.Duration) time.Duration { return d.Round(100 * time.Millisecond) }
