// Package queue plays tracks one after another, with one queue per guild and
// one guild in voice at a time (the VPS keeps a single voice connection).
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"bot/internal/voice"
)

// ErrFull means the guild's queue already holds the maximum number of tracks.
var ErrFull = errors.New("queue is full")

// Loaded is a track ready to play.
type Loaded struct {
	Path     string
	Title    string
	Duration time.Duration
}

// LoadFunc fetches a track's audio (e.g. from the cache, downloading if needed).
type LoadFunc func(ctx context.Context) (Loaded, error)

// Track is one queued request.
type Track struct {
	Title        string        // may be empty until loaded
	URL          string        // shown when there is no title yet
	Duration     time.Duration // 0 if unknown
	RequestedBy  snowflake.ID
	VoiceChannel snowflake.ID // where the requester was when asking
	TextChannel  snowflake.ID // where status messages go
	Load         LoadFunc
}

// Name is the track's display name, safe to put in a Discord message.
func (t Track) Name() string {
	switch {
	case t.Title != "":
		return "**" + EscapeMarkdown(t.Title) + "**"
	case t.URL != "":
		return "<" + t.URL + ">" // angle brackets stop Discord from embedding it
	default:
		return "an untitled track"
	}
}

// Notifier posts status messages. Messages must not ping anyone.
type Notifier interface {
	Notify(channelID snowflake.ID, content string)
}

// Position describes where a newly queued track landed.
type Position struct {
	Ahead      int  // tracks before it, including the one playing
	OtherGuild bool // another server is using the voice connection; this queue will wait
}

// Listing is a snapshot of a guild's queue.
type Listing struct {
	Current  *Track
	Upcoming []Track
}

// Options configures a Manager.
type Options struct {
	Connector voice.Connector
	Clock     voice.Clock // nil means real time
	Notifier  Notifier
	Logger    *slog.Logger
	MaxLength int // per guild, including the playing track; <1 means 10
}

// Manager owns all guild queues.
type Manager struct {
	base      context.Context
	connector voice.Connector
	clock     voice.Clock
	notifier  Notifier
	logger    *slog.Logger
	maxLen    int
	slot      chan struct{} // the single voice connection

	mu     sync.Mutex
	guilds map[snowflake.ID]*guildQueue
	active snowflake.ID // guild holding the slot, 0 if none
	wg     sync.WaitGroup
}

type guildQueue struct {
	pending []*item
	current *item
	running bool // a worker goroutine owns this queue
	// connected is true while the bot holds a voice connection it opened itself.
	// It is cleared before the bot leaves on its own, so a later "left voice"
	// event can be told apart from being disconnected by someone else.
	connected bool
	kicked    bool // disconnected by someone else; the queue was discarded
}

type item struct {
	Track
	done   chan struct{} // closed when Load finishes
	loaded Loaded
	err    error
	cancel context.CancelFunc // set while playing
}

// New builds a Manager. base bounds everything: cancel it on shutdown, then call Wait.
func New(base context.Context, opts Options) *Manager {
	m := &Manager{
		base:      base,
		connector: opts.Connector,
		clock:     opts.Clock,
		notifier:  opts.Notifier,
		logger:    opts.Logger,
		maxLen:    opts.MaxLength,
		slot:      make(chan struct{}, 1),
		guilds:    map[snowflake.ID]*guildQueue{},
	}
	if m.maxLen < 1 {
		m.maxLen = 10
	}
	if m.clock == nil {
		m.clock = voice.RealClock{}
	}
	if m.logger == nil {
		m.logger = slog.New(slog.DiscardHandler)
	}
	return m
}

// Wait blocks until all playback and loading has stopped (after base is cancelled).
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) guild(id snowflake.ID) *guildQueue {
	g, ok := m.guilds[id]
	if !ok {
		g = &guildQueue{}
		m.guilds[id] = g
	}
	return g
}

// Enqueue adds t to the guild's queue and starts loading it immediately.
func (m *Manager) Enqueue(guildID snowflake.ID, t Track) (Position, error) {
	if err := m.base.Err(); err != nil {
		return Position{}, err
	}
	m.mu.Lock()
	g := m.guild(guildID)
	ahead := len(g.pending)
	if g.current != nil {
		ahead++
	}
	if ahead >= m.maxLen {
		m.mu.Unlock()
		return Position{}, ErrFull
	}
	it := &item{Track: t, done: make(chan struct{})}
	g.pending = append(g.pending, it)
	pos := Position{Ahead: ahead, OtherGuild: m.active != 0 && m.active != guildID}
	if !g.running {
		g.running = true
		m.wg.Add(1)
		go m.worker(guildID, g)
	}
	m.wg.Add(1)
	m.mu.Unlock()

	go m.load(guildID, it)
	return pos, nil
}

// load runs a track's LoadFunc. A track that fails while still waiting in the
// queue is removed and its requester told right away; one that fails as it
// comes up to play is reported by the worker instead.
func (m *Manager) load(guildID snowflake.ID, it *item) {
	defer m.wg.Done()
	loaded, err := it.Load(m.base)

	m.mu.Lock()
	it.loaded, it.err = loaded, err
	if err == nil {
		if loaded.Title != "" {
			it.Title = loaded.Title
		}
		if loaded.Duration > 0 {
			it.Duration = loaded.Duration
		}
	}
	removed := err != nil && m.base.Err() == nil && m.removePending(guildID, it)
	name := it.Name()
	close(it.done)
	m.mu.Unlock()

	if removed {
		m.logger.Warn("queue: track failed to load", "guild", guildID, "track", it.URL, "err", err)
		m.notify(it.TextChannel, fmt.Sprintf("Couldn't add %s: %s", name, UserMessage(err)))
	}
}

// removePending drops it from the guild's pending list. Caller holds m.mu.
func (m *Manager) removePending(guildID snowflake.ID, it *item) bool {
	g := m.guilds[guildID]
	for i, p := range g.pending {
		if p == it {
			g.pending = append(g.pending[:i], g.pending[i+1:]...)
			return true
		}
	}
	return false
}

// session is a guild's voice connection while its worker runs.
type session struct {
	m       *Manager
	g       *guildQueue
	guildID snowflake.ID
	log     *slog.Logger
	conn    voice.Connection
	channel snowflake.ID
}

// leave closes the connection. connected is cleared first, so the resulting
// "bot left voice" event is not mistaken for a disconnect by someone else.
func (s *session) leave() {
	if s.conn == nil {
		return
	}
	s.m.mu.Lock()
	s.g.connected = false
	s.m.mu.Unlock()
	closeConn(s.conn)
	s.conn = nil
	s.log.Info("queue: left voice", "channel", s.channel)
}

// worker plays the guild's queue until it is empty, then leaves voice.
func (m *Manager) worker(guildID snowflake.ID, g *guildQueue) {
	defer m.wg.Done()
	s := &session{m: m, g: g, guildID: guildID, log: m.logger.With("guild", guildID)}

	select {
	case m.slot <- struct{}{}: // wait while another guild is in voice
	case <-m.base.Done():
		m.mu.Lock()
		g.running, g.pending = false, nil
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.active = guildID
	m.mu.Unlock()
	defer func() {
		s.leave()
		<-m.slot
	}()

	for {
		m.mu.Lock()
		if g.kicked {
			// Discord already dropped us; closing just releases our side (DAVE session).
			g.kicked = false
			m.mu.Unlock()
			s.leave()
			m.mu.Lock()
		}
		if len(g.pending) == 0 || m.base.Err() != nil {
			// Decided under the lock, so a concurrent Enqueue either lands before
			// this check or sees running=false and starts a new worker.
			g.running, g.pending = false, nil
			if m.active == guildID {
				m.active = 0
			}
			m.mu.Unlock()
			return
		}
		it := g.pending[0]
		g.pending = g.pending[1:]
		ctx, cancel := context.WithCancel(m.base)
		it.cancel = cancel
		g.current = it
		m.mu.Unlock()

		s.play(ctx, it)
		cancel()

		m.mu.Lock()
		g.current = nil
		m.mu.Unlock()
	}
}

// play waits for one track to load, joins its voice channel if needed, and plays it.
func (s *session) play(ctx context.Context, it *item) {
	m, log := s.m, s.log
	select {
	case <-it.done:
	case <-ctx.Done():
		return // skipped or stopped while still loading
	}
	m.mu.Lock()
	loaded, err, name := it.loaded, it.err, it.Name()
	duration := it.Duration
	m.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("queue: track failed to load", "track", it.URL, "err", err)
			m.notify(it.TextChannel, fmt.Sprintf("Couldn't play %s: %s", name, UserMessage(err)))
		}
		return
	}

	if s.conn == nil || s.channel != it.VoiceChannel {
		s.leave()
		c, err := voice.Join(ctx, m.connector, s.guildID, it.VoiceChannel)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("queue: could not join voice", "channel", it.VoiceChannel, "err", err)
				msg := "Couldn't join the voice channel, so I skipped %s."
				if errors.Is(err, voice.ErrNotReady) {
					msg = "Voice encryption (DAVE) didn't finish setting up, so I skipped %s."
				}
				m.notify(it.TextChannel, fmt.Sprintf(msg, name))
			}
			return
		}
		s.conn, s.channel = c, it.VoiceChannel
		m.mu.Lock()
		s.g.connected = true
		m.mu.Unlock()
		log.Info("queue: joined voice", "channel", it.VoiceChannel)
	}

	m.notify(it.TextChannel, fmt.Sprintf("Now playing: %s%s, requested by <@%s>.", name, formatLength(duration), it.RequestedBy))
	frames, err := voice.PlayFile(ctx, m.clock, s.conn, loaded.Path)
	log.Info("queue: track finished", "track", it.URL, "frames", frames, "err", err)
	if err != nil && ctx.Err() == nil {
		m.notify(it.TextChannel, fmt.Sprintf("Playback of %s failed. Details are in the bot's log.", name))
		s.leave() // the connection may be broken; rejoin for the next track
	}
}

// Disconnected tells the queue the bot was removed from voice in guildID by
// someone else (kicked, or its channel deleted). The guild's queue is
// discarded. It reports whether a queue was affected; events caused by the
// bot leaving on its own are ignored.
func (m *Manager) Disconnected(guildID snowflake.ID) bool {
	m.mu.Lock()
	g := m.guilds[guildID]
	if g == nil || !g.connected {
		m.mu.Unlock()
		return false
	}
	g.connected, g.kicked = false, true
	g.pending = nil
	var textChannel snowflake.ID
	if g.current != nil {
		textChannel = g.current.TextChannel
		g.current.cancel()
	}
	m.mu.Unlock()

	m.logger.Info("queue: disconnected from voice by someone else; queue discarded", "guild", guildID)
	m.notify(textChannel, "I was disconnected from voice, so I cleared the queue.")
	return true
}

// Skip stops the playing track; the next one starts. It returns the skipped track.
func (m *Manager) Skip(guildID snowflake.ID) (Track, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.guilds[guildID]
	if g == nil || g.current == nil {
		return Track{}, false
	}
	g.current.cancel()
	return g.current.Track, true
}

// Stop clears the guild's queue and stops playback; the bot then leaves voice.
// It reports whether there was anything to stop.
func (m *Manager) Stop(guildID snowflake.ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.guilds[guildID]
	if g == nil || (g.current == nil && len(g.pending) == 0) {
		return false
	}
	g.pending = nil
	if g.current != nil {
		g.current.cancel()
	}
	return true
}

// List returns the guild's queue.
func (m *Manager) List(guildID snowflake.ID) Listing {
	m.mu.Lock()
	defer m.mu.Unlock()
	var l Listing
	g := m.guilds[guildID]
	if g == nil {
		return l
	}
	if g.current != nil {
		t := g.current.Track
		l.Current = &t
	}
	for _, it := range g.pending {
		l.Upcoming = append(l.Upcoming, it.Track)
	}
	return l
}

func (m *Manager) notify(channelID snowflake.ID, content string) {
	if m.notifier != nil && channelID != 0 {
		m.notifier.Notify(channelID, content)
	}
}

func closeConn(c voice.Connection) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Close(ctx)
}

// UserMessage explains err to a Discord user. Errors that know how to explain
// themselves (e.g. yt-dlp failures) implement UserMessage() string.
func UserMessage(err error) string {
	var um interface{ UserMessage() string }
	if errors.As(err, &um) {
		return um.UserMessage()
	}
	return "something went wrong (details are in the bot's log)."
}

// FormatDuration renders d as m:ss or h:mm:ss.
func FormatDuration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// formatLength is " (m:ss)" or "" when unknown.
func formatLength(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return " (" + FormatDuration(d) + ")"
}

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, "`", "\\`", `~`, `\~`, `|`, `\|`, `>`, `\>`, `#`, `\#`, `[`, `\[`, `]`, `\]`,
)

// EscapeMarkdown stops user-controlled text (e.g. video titles) from being
// rendered as Discord markdown.
func EscapeMarkdown(s string) string { return markdownEscaper.Replace(s) }
