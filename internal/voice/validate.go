package voice

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ValidateFile checks that path is Ogg Opus the bot can pass straight through:
// every packet exactly 20 ms. The cache runs it before admitting a download, so
// a bad file is rejected once instead of failing on every play.
func ValidateFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	src, err := newFrameSource(f)
	if err != nil {
		return err
	}
	n := 0
	for {
		_, err := src.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("packet %d: %w", n, err)
		}
		n++
	}
	if n == 0 {
		return errors.New("no audio packets")
	}
	return nil
}
