package voice

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateFile(t *testing.T) {
	if err := ValidateFile(fixture); err != nil {
		t.Errorf("real fixture: %v", err)
	}

	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tenMs := newStream()
	tenMs.page([][]byte{frame20(8, 1), frame20(8, 2), {30 << 3, 0}}, false) // third packet is 10 ms
	empty := newStream()

	for name, path := range map[string]string{
		"10 ms frame": write("10ms.opus", tenMs.buf.Bytes()),
		"no packets":  write("empty.opus", empty.buf.Bytes()),
		"not ogg":     write("x.opus", []byte("<html>rate limited</html>")),
		"missing":     filepath.Join(dir, "nope.opus"),
	} {
		if err := ValidateFile(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInspectFileCountsPackets(t *testing.T) {
	s := newStream()
	s.page([][]byte{frame20(8, 1), frame20(8, 2), frame20(8, 3)}, false)
	p := filepath.Join(t.TempDir(), "three.opus")
	if err := os.WriteFile(p, s.buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, err := InspectFile(p); err != nil || d != 3*FrameDuration {
		t.Errorf("InspectFile = %v, %v; want 60ms", d, err)
	}
}

// The committed test tone (assets/tone.opus) must be playable.
func TestCommittedTestToneIsValid(t *testing.T) {
	if err := ValidateFile(filepath.Join("..", "..", "assets", "tone.opus")); err != nil {
		t.Errorf("assets/tone.opus: %v", err)
	}
}
