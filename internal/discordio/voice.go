package discordio

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/disgoorg/disgo/cache"
	disgovoice "github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/thomas-vilte/dave-go/session"

	"github.com/kirkmadraga/vox-engine/internal/voice"
)

// ErrNoDAVE means a voice connection was created without a dave-go session,
// i.e. DAVE is not wired and Discord would reject the audio.
var ErrNoDAVE = errors.New("no DAVE session for voice connection (DAVE not wired)")

// DAVE wires dave-go into disgo's voice manager and hands each new session to
// the connection that created it. disgo never closes DAVE sessions itself, so
// the connection does it on leave.
//
// disgo creates the session synchronously inside VoiceManager.CreateConn
// (NewConn calls the session create func), so the session to take is the one
// the hook saw during that call. createConn runs CreateConn and the take under
// one lock, so servers joining at the same time can't swap sessions.
type DAVE struct {
	connectMu sync.Mutex // one CreateConn+take at a time

	mu      sync.Mutex
	pending *session.Session
}

// createConn calls create and returns the connection with the DAVE session
// created during that call.
func (d *DAVE) createConn(create func() disgovoice.Conn) (disgovoice.Conn, *session.Session) {
	d.connectMu.Lock()
	defer d.connectMu.Unlock()
	conn := create()
	return conn, d.take()
}

// CreateFunc is passed to voice.WithDaveSessionCreateFunc.
func (d *DAVE) CreateFunc() godave.SessionCreateFunc {
	return session.CreateFunc(session.WithSessionHook(func(s *session.Session) {
		d.mu.Lock()
		d.pending = s
		d.mu.Unlock()
	}))
}

func (d *DAVE) take() *session.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.pending
	d.pending = nil
	return s
}

// VoiceConnector implements voice.Connector over disgo's voice manager.
type VoiceConnector struct {
	Manager disgovoice.Manager
	DAVE    *DAVE
}

func (c VoiceConnector) Connect(ctx context.Context, guildID, channelID snowflake.ID) (voice.Connection, error) {
	conn, sess := c.DAVE.createConn(func() disgovoice.Conn { return c.Manager.CreateConn(guildID) })
	vc := &voiceConn{conn: conn, dave: sess}
	if sess == nil {
		vc.Close(ctx)
		return nil, ErrNoDAVE
	}
	// Self-deafened: the bot never needs to receive audio.
	if err := conn.Open(ctx, channelID, false, true); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		vc.Close(closeCtx)
		return nil, err
	}
	return vc, nil
}

type voiceConn struct {
	conn disgovoice.Conn
	dave *session.Session
}

func (v *voiceConn) WaitReady(ctx context.Context) error {
	_, err := v.dave.WaitReady(ctx)
	return err
}

func (v *voiceConn) SetSpeaking(ctx context.Context, speaking bool) error {
	flags := disgovoice.SpeakingFlagNone
	if speaking {
		flags = disgovoice.SpeakingFlagMicrophone
	}
	return v.conn.SetSpeaking(ctx, flags)
}

// WriteOpus sends one frame; disgo's UDP layer DAVE-encrypts it.
func (v *voiceConn) WriteOpus(frame []byte) error {
	_, err := v.conn.UDP().Write(frame)
	return err
}

func (v *voiceConn) Close(ctx context.Context) {
	v.conn.Close(ctx)
	if v.dave != nil {
		v.dave.Close()
	}
}

// VoiceStates finds users' current voice channels from disgo's voice-state
// cache (needs the GuildVoiceStates intent and cache.FlagVoiceStates).
type VoiceStates struct {
	Caches cache.Caches
}

// UserVoiceChannel returns the voice channel userID is in, in guildID.
func (v VoiceStates) UserVoiceChannel(guildID, userID snowflake.ID) (snowflake.ID, bool) {
	vs, ok := v.Caches.VoiceState(guildID, userID)
	if !ok || vs.ChannelID == nil {
		return 0, false
	}
	return *vs.ChannelID, true
}
