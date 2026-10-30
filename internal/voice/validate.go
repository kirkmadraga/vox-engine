package voice

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ValidateFile checks that path is Ogg Opus the bot can pass straight through:
// every packet exactly 20 ms. The cache runs it before admitting a download, so
// a bad file is rejected once instead of failing on every play.
func ValidateFile(path string) error {
	_, err := InspectFile(path)
	return err
}

// InspectFile validates path like ValidateFile and returns its length, from
// the packet count (every packet is 20 ms).
func InspectFile(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	src, err := newFrameSource(f)
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		_, err := src.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("packet %d: %w", n, err)
		}
		n++
	}
	if n == 0 {
		return 0, errors.New("no audio packets")
	}
	return time.Duration(n) * FrameDuration, nil
}
