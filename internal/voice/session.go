package voice

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// ErrNotReady means DAVE end-to-end encryption did not become active in time.
var ErrNotReady = errors.New("voice encryption (DAVE) did not become ready")

// Connection is one live voice connection. The disgo adapter implements it;
// tests use a fake.
type Connection interface {
	// WaitReady blocks until DAVE end-to-end encryption is active.
	WaitReady(ctx context.Context) error
	SetSpeaking(ctx context.Context, speaking bool) error
	// WriteOpus sends one 20 ms Opus frame (encrypted by the connection).
	WriteOpus(frame []byte) error
	Close(ctx context.Context)
}

// Connector opens voice connections.
type Connector interface {
	Connect(ctx context.Context, guildID, channelID snowflake.ID) (Connection, error)
}

// Timeouts for joining.
const (
	ConnectTimeout = 15 * time.Second
	ReadyTimeout   = 15 * time.Second
	trailingSilent = 5 // silence frames after a track, so its last packet isn't cut off
)

// silenceFrame is an Opus packet of 20 ms silence.
var silenceFrame = []byte{0xF8, 0xFF, 0xFE}

// Join connects to a voice channel and waits until DAVE is ready. On any
// failure the connection is closed before returning.
func Join(ctx context.Context, connector Connector, guildID, channelID snowflake.ID) (Connection, error) {
	cctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	conn, err := connector.Connect(cctx, guildID, channelID)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("join voice channel: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, ReadyTimeout)
	err = conn.WaitReady(rctx)
	cancel()
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(closeCtx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	return conn, nil
}

// PlayFile plays one Ogg Opus file on an open connection, paced at 20 ms, then
// sends a little silence and clears the speaking flag. It returns the number
// of audio frames sent. Cancelling ctx stops playback promptly (skip, stop,
// shutdown); the connection stays open for the caller to reuse or close.
func PlayFile(ctx context.Context, clock Clock, conn Connection, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	src, err := newFrameSource(f)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if clock == nil {
		clock = RealClock{}
	}

	if err := conn.SetSpeaking(ctx, true); err != nil {
		return 0, fmt.Errorf("set speaking: %w", err)
	}
	frames, err := Pace(ctx, clock, FrameDuration, src.next, conn.WriteOpus)

	// Always end cleanly, even after a skip: detach from ctx for these few frames.
	tail, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	silence := trailingSilent
	_, _ = Pace(tail, clock, FrameDuration, func() ([]byte, error) {
		if silence == 0 {
			return nil, io.EOF
		}
		silence--
		return silenceFrame, nil
	}, conn.WriteOpus)
	_ = conn.SetSpeaking(tail, false)
	return frames, err
}

// frameSource yields Opus packets, rejecting any that are not exactly 20 ms.
type frameSource struct {
	ogg *OggReader
}

func newFrameSource(r io.Reader) (*frameSource, error) {
	ogg, err := NewOggReader(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	return &frameSource{ogg: ogg}, nil
}

func (s *frameSource) next() ([]byte, error) {
	p, err := s.ogg.NextPacket()
	if err != nil {
		return nil, err
	}
	d, err := PacketDuration(p)
	if err != nil {
		return nil, err
	}
	if d != FrameDuration {
		return nil, fmt.Errorf("unsupported Opus frame duration %v (need %v)", d, FrameDuration)
	}
	return p, nil
}
