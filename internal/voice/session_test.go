package voice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

type fakeConn struct {
	mu       sync.Mutex
	readyErr error
	onWrite  func(n int) // called after each frame, with the running count
	events   []string
	frames   int
	closed   bool
}

func (c *fakeConn) log(e string) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *fakeConn) WaitReady(context.Context) error { c.log("ready"); return c.readyErr }
func (c *fakeConn) SetSpeaking(_ context.Context, on bool) error {
	if on {
		c.log("speaking")
	} else {
		c.log("silent")
	}
	return nil
}
func (c *fakeConn) WriteOpus([]byte) error {
	c.mu.Lock()
	c.frames++
	n := c.frames
	c.mu.Unlock()
	if c.onWrite != nil {
		c.onWrite(n)
	}
	return nil
}
func (c *fakeConn) Close(context.Context) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.log("close")
}

type fakeConnector struct {
	conn           *fakeConn
	err            error
	guild, channel snowflake.ID
}

func (f *fakeConnector) Connect(_ context.Context, g, ch snowflake.ID) (Connection, error) {
	f.guild, f.channel = g, ch
	if f.err != nil {
		return nil, f.err
	}
	return f.conn, nil
}

const fixture = "testdata/tone-1s.opus"

func TestJoin(t *testing.T) {
	conn := &fakeConn{}
	connector := &fakeConnector{conn: conn}
	got, err := Join(context.Background(), connector, 10, 20)
	if err != nil || got != conn {
		t.Fatalf("Join = %v, %v", got, err)
	}
	if connector.guild != 10 || connector.channel != 20 || conn.closed {
		t.Errorf("guild=%d channel=%d closed=%v", connector.guild, connector.channel, conn.closed)
	}
}

func TestJoinDAVENotReadyClosesConnection(t *testing.T) {
	conn := &fakeConn{readyErr: errors.New("handshake timeout")}
	if _, err := Join(context.Background(), &fakeConnector{conn: conn}, 1, 2); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
	if !conn.closed {
		t.Error("connection must be closed when DAVE never becomes ready")
	}
}

func TestJoinConnectError(t *testing.T) {
	if _, err := Join(context.Background(), &fakeConnector{err: errors.New("no permission")}, 1, 2); err == nil {
		t.Error("expected error")
	}
}

func TestPlayFileWholeFile(t *testing.T) {
	conn := &fakeConn{}
	frames, err := PlayFile(context.Background(), newFakeClock(), conn, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if frames < 50 || frames > 51 {
		t.Errorf("audio frames = %d, want 50-51", frames)
	}
	if conn.frames != frames+trailingSilent {
		t.Errorf("sent %d frames, want %d audio + %d silence", conn.frames, frames, trailingSilent)
	}
	if want := []string{"speaking", "silent"}; !slices.Equal(conn.events, want) {
		t.Errorf("events = %v, want %v", conn.events, want)
	}
	if conn.closed {
		t.Error("PlayFile must leave the connection open for the next track")
	}
}

func TestPlayFileSkipStopsPromptlyAndEndsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &fakeConn{onWrite: func(n int) {
		if n == 10 {
			cancel() // skip after 10 frames
		}
	}}
	frames, err := PlayFile(ctx, newFakeClock(), conn, fixture)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if frames != 10 {
		t.Errorf("audio frames = %d, want 10", frames)
	}
	if conn.frames != 10+trailingSilent || conn.events[len(conn.events)-1] != "silent" {
		t.Errorf("frames=%d events=%v: a skip should still end with silence and speaking off", conn.frames, conn.events)
	}
}

func TestPlayFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := PlayFile(context.Background(), newFakeClock(), &fakeConn{}, filepath.Join(dir, "missing.opus")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}

	w := newStream()
	w.page([][]byte{frame20(8, 1), {30 << 3, 0, 0}}, false) // second packet is 10 ms
	bad := filepath.Join(dir, "10ms.opus")
	os.WriteFile(bad, w.buf.Bytes(), 0o644)
	conn := &fakeConn{}
	frames, err := PlayFile(context.Background(), newFakeClock(), conn, bad)
	if err == nil || frames != 1 {
		t.Errorf("non-20ms: frames=%d err=%v, want stop after 1 frame", frames, err)
	}
}
