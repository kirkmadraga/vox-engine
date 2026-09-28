package voice

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Errors from Player.
var (
	ErrBusy     = errors.New("already playing")
	ErrNotReady = errors.New("voice encryption (DAVE) did not become ready")
)

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

// Timeouts for the stages of a playback job.
const (
	connectTimeout = 15 * time.Second
	readyTimeout   = 15 * time.Second
	closeTimeout   = 5 * time.Second
	trailingSilent = 5 // silence frames after the audio, so the last packet isn't cut off
)

// silenceFrame is an Opus packet of 20 ms silence.
var silenceFrame = []byte{0xF8, 0xFF, 0xFE}

// Player plays one Ogg Opus file at a time into a voice channel, joining
// before and leaving after. Playback runs in the background, bound to the
// Player's base context (cancel it on shutdown, then call Wait).
type Player struct {
	base      context.Context
	connector Connector
	clock     Clock
	logger    *slog.Logger

	mu   sync.Mutex
	busy bool
	wg   sync.WaitGroup
}

// NewPlayer builds a Player. clock and logger may be nil.
func NewPlayer(base context.Context, connector Connector, clock Clock, logger *slog.Logger) *Player {
	if clock == nil {
		clock = RealClock{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Player{base: base, connector: connector, clock: clock, logger: logger}
}

// Start begins playing the Ogg Opus file at path in the given voice channel.
// File problems are returned immediately; ErrBusy if something is already
// playing. Otherwise playback runs in the background and done is called once
// with its result (nil on success).
func (p *Player) Start(guildID, channelID snowflake.ID, path string, done func(error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy {
		return ErrBusy
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	src, err := newFrameSource(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("%s: %w", path, err)
	}

	p.busy = true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		err := p.play(guildID, channelID, src)
		f.Close() // close before reporting, so callers may delete or rename the file
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
		if done != nil {
			done(err)
		}
	}()
	return nil
}

// Wait blocks until background playback has finished.
func (p *Player) Wait() { p.wg.Wait() }

func (p *Player) play(guildID, channelID snowflake.ID, src *frameSource) (err error) {
	log := p.logger.With("guild", guildID, "channel", channelID)
	ctx := p.base

	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := p.connector.Connect(cctx, guildID, channelID)
	cancel()
	if err != nil {
		return fmt.Errorf("join voice channel: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		conn.Close(closeCtx)
		log.Info("voice: left channel")
	}()

	rctx, cancel := context.WithTimeout(ctx, readyTimeout)
	start := p.clock.Now()
	err = conn.WaitReady(rctx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Error("voice: DAVE not ready", "err", err)
		return fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	log.Info("voice: DAVE ready", "after", p.clock.Now().Sub(start))

	if err := conn.SetSpeaking(ctx, true); err != nil {
		return fmt.Errorf("set speaking: %w", err)
	}
	frames, err := Pace(ctx, p.clock, FrameDuration, src.next, conn.WriteOpus)
	log.Info("voice: playback finished", "frames", frames, "seconds", float64(frames)*FrameDuration.Seconds(), "err", err)
	if err != nil {
		return err
	}

	silence := trailingSilent
	_, _ = Pace(ctx, p.clock, FrameDuration, func() ([]byte, error) {
		if silence == 0 {
			return nil, io.EOF
		}
		silence--
		return silenceFrame, nil
	}, conn.WriteOpus)
	_ = conn.SetSpeaking(ctx, false)
	return nil
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
