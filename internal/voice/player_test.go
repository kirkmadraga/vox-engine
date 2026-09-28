package voice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

type fakeConn struct {
	mu       sync.Mutex
	readyErr error
	blockOn  chan struct{} // if set, WriteOpus blocks until closed (to hold playback open)
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
	if c.blockOn != nil {
		<-c.blockOn
	}
	c.mu.Lock()
	c.frames++
	c.mu.Unlock()
	return nil
}
func (c *fakeConn) Close(context.Context) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.log("close")
}

type fakeConnector struct {
	conn    *fakeConn
	err     error
	guild   snowflake.ID
	channel snowflake.ID
}

func (f *fakeConnector) Connect(_ context.Context, g, ch snowflake.ID) (Connection, error) {
	f.guild, f.channel = g, ch
	if f.err != nil {
		return nil, f.err
	}
	return f.conn, nil
}

const fixture = "testdata/tone-1s.opus"

// startAndWait runs one playback and returns its result.
func startAndWait(t *testing.T, p *Player, path string) error {
	t.Helper()
	result := make(chan error, 1)
	if err := p.Start(10, 20, path, func(err error) { result <- err }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("playback did not finish")
		return nil
	}
}

func TestPlayerPlaysWholeFileThenLeaves(t *testing.T) {
	conn := &fakeConn{}
	connector := &fakeConnector{conn: conn}
	p := NewPlayer(context.Background(), connector, newFakeClock(), nil)

	if err := startAndWait(t, p, fixture); err != nil {
		t.Fatalf("playback: %v", err)
	}
	if connector.guild != 10 || connector.channel != 20 {
		t.Errorf("connected to %d/%d", connector.guild, connector.channel)
	}
	// 50-51 audio frames plus trailing silence.
	if conn.frames < 50+trailingSilent || conn.frames > 51+trailingSilent {
		t.Errorf("frames = %d", conn.frames)
	}
	want := []string{"ready", "speaking", "silent", "close"}
	if len(conn.events) != len(want) {
		t.Fatalf("events = %v, want %v", conn.events, want)
	}
	for i := range want {
		if conn.events[i] != want[i] {
			t.Fatalf("events = %v, want %v", conn.events, want)
		}
	}
}

func TestPlayerBusy(t *testing.T) {
	release := make(chan struct{})
	conn := &fakeConn{blockOn: release}
	p := NewPlayer(context.Background(), &fakeConnector{conn: conn}, newFakeClock(), nil)

	done := make(chan error, 1)
	if err := p.Start(10, 20, fixture, func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(10, 20, fixture, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("second Start: err = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Free again once the first job is done.
	if err := startAndWait(t, p, fixture); err != nil {
		t.Errorf("third Start after finish: %v", err)
	}
}

func TestPlayerFileErrorsAreImmediate(t *testing.T) {
	p := NewPlayer(context.Background(), &fakeConnector{conn: &fakeConn{}}, newFakeClock(), nil)
	if err := p.Start(1, 2, filepath.Join(t.TempDir(), "missing.opus"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v", err)
	}
	notOgg := filepath.Join(t.TempDir(), "x.opus")
	os.WriteFile(notOgg, []byte("definitely not ogg"), 0o644)
	if err := p.Start(1, 2, notOgg, nil); err == nil {
		t.Error("expected error for non-Ogg file")
	}
	// A failed Start must not leave the player busy.
	if err := startAndWait(t, p, fixture); err != nil {
		t.Errorf("start after failures: %v", err)
	}
}

func TestPlayerDAVENotReadyLeaves(t *testing.T) {
	conn := &fakeConn{readyErr: errors.New("handshake timeout")}
	p := NewPlayer(context.Background(), &fakeConnector{conn: conn}, newFakeClock(), nil)
	err := startAndWait(t, p, fixture)
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
	if !conn.closed || conn.frames != 0 {
		t.Errorf("closed=%v frames=%d: must leave without sending audio", conn.closed, conn.frames)
	}
}

func TestPlayerConnectError(t *testing.T) {
	p := NewPlayer(context.Background(), &fakeConnector{err: errors.New("no permission")}, newFakeClock(), nil)
	if err := startAndWait(t, p, fixture); err == nil {
		t.Error("expected connect error")
	}
}

func TestPlayerShutdownStopsAndLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	conn := &fakeConn{blockOn: release}
	p := NewPlayer(ctx, &fakeConnector{conn: conn}, newFakeClock(), nil)

	done := make(chan error, 1)
	p.Start(10, 20, fixture, func(err error) { done <- err })
	cancel()       // shutdown
	close(release) // let the in-flight write return
	p.Wait()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !conn.closed {
		t.Error("connection not closed on shutdown")
	}
	if conn.frames > 2 {
		t.Errorf("kept playing after shutdown: %d frames", conn.frames)
	}
}

func TestPlayerRejectsNon20msFrames(t *testing.T) {
	w := newStream()
	w.page([][]byte{frame20(8, 1), {30 << 3, 0, 0}}, false) // second packet is 10 ms
	path := filepath.Join(t.TempDir(), "10ms.opus")
	os.WriteFile(path, w.buf.Bytes(), 0o644)

	conn := &fakeConn{}
	p := NewPlayer(context.Background(), &fakeConnector{conn: conn}, newFakeClock(), nil)
	if err := startAndWait(t, p, path); err == nil {
		t.Error("expected frame duration error")
	}
	if conn.frames != 1 || !conn.closed {
		t.Errorf("frames=%d closed=%v: should stop at the bad frame and leave", conn.frames, conn.closed)
	}
}
