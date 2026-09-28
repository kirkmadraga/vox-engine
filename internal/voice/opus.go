package voice

import (
	"errors"
	"time"
)

// FrameDuration is the only Opus packet duration Discord accepts from us:
// disgo stamps every packet as 960 samples (20 ms at 48 kHz).
const FrameDuration = 20 * time.Millisecond

// PacketDuration returns the audio duration of one Opus packet, from its TOC
// byte (RFC 6716 section 3.1).
func PacketDuration(p []byte) (time.Duration, error) {
	if len(p) == 0 {
		return 0, errors.New("opus: empty packet")
	}
	config := p[0] >> 3
	var frame time.Duration
	switch {
	case config < 12: // SILK-only: 10, 20, 40, 60 ms
		frame = [...]time.Duration{10, 20, 40, 60}[config%4] * time.Millisecond
	case config < 16: // Hybrid: 10, 20 ms
		frame = [...]time.Duration{10, 20}[config%2] * time.Millisecond
	default: // CELT-only: 2.5, 5, 10, 20 ms
		frame = [...]time.Duration{2500, 5000, 10000, 20000}[config%4] * time.Microsecond
	}

	var frames int
	switch p[0] & 3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0, errors.New("opus: truncated code 3 packet")
		}
		frames = int(p[1] & 0x3f)
		if frames == 0 {
			return 0, errors.New("opus: code 3 packet with zero frames")
		}
	}
	return time.Duration(frames) * frame, nil
}
