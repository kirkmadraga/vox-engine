package voice

import (
	"testing"
	"time"
)

func TestPacketDuration(t *testing.T) {
	toc := func(config, code byte) byte { return config<<3 | code }
	ms := time.Millisecond
	cases := []struct {
		name string
		p    []byte
		want time.Duration
	}{
		{"CELT 20ms", []byte{toc(31, 0)}, 20 * ms},
		{"CELT 10ms", []byte{toc(30, 0)}, 10 * ms},
		{"CELT 2.5ms", []byte{toc(16, 0)}, 2500 * time.Microsecond},
		{"CELT 2x10ms (code 1)", []byte{toc(30, 1)}, 20 * ms},
		{"CELT 2x20ms (code 2)", []byte{toc(31, 2)}, 40 * ms},
		{"CELT 3x20ms (code 3)", []byte{toc(31, 3), 3}, 60 * ms},
		{"SILK 20ms", []byte{toc(1, 0)}, 20 * ms},
		{"SILK 60ms", []byte{toc(3, 0)}, 60 * ms},
		{"Hybrid 20ms", []byte{toc(13, 0)}, 20 * ms},
		{"Hybrid 10ms", []byte{toc(14, 0)}, 10 * ms},
		{"silence frame", silenceFrame, 20 * ms},
	}
	for _, c := range cases {
		got, err := PacketDuration(c.p)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	for name, p := range map[string][]byte{"empty": {}, "code 3 truncated": {toc(31, 3)}, "code 3 zero frames": {toc(31, 3), 0}} {
		if _, err := PacketDuration(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
