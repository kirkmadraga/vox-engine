// Package queue plays tracks one after another, with one queue per guild and
// at most MaxSessions guilds in voice at a time (others wait for a slot).
package queue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/voice"
)

// ErrFull means the guild's queue already holds the maximum number of tracks.
var ErrFull = errors.New("queue is full")

// Loaded is a track ready to play.
type Loaded struct {
	Path     string
	Title    string
	Duration time.Duration
	OnStart  func() // optional: called when the track starts playing (e.g. to record the play)
}

// LoadFunc fetches a track's audio (e.g. from the cache, downloading if needed).
type LoadFunc func(ctx context.Context) (Loaded, error)

// Track is one queued request.
type Track struct {
	Key          string        // cache key (video ID); queued and playing keys are reported by InUse
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
	OtherGuild bool // every voice slot is busy playing in other servers; this queue will wait
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
	// MaxSessions is how many guilds can be in voice at the same time; <1 means 1.
	MaxSessions int
	// IdleTimeout is how long to stay in voice after a guild's queue runs out,
	// so a new request plays without rejoining; 0 leaves at once. An idle guild
	// leaves early when another guild is waiting for a voice slot.
	IdleTimeout time.Duration
	// After is the idle timer; nil means time.After. Tests replace it.
	After func(time.Duration) <-chan time.Time
}

// Manager owns all guild queues.
type Manager struct {
	base      context.Context
	connector voice.Connector
	clock     voice.Clock
	notifier  Notifier
	logger    *slog.Logger
	maxLen    int
	slot      chan struct{} // one token per guild in voice
	idle      time.Duration
	after     func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	guilds  map[snowflake.ID]*guildQueue
	active  map[snowflake.ID]bool // guilds holding a voice slot
	waiting int                   // workers waiting for a voice slot
	closed  bool                  // Wait has begun: start nothing new
	wg      sync.WaitGroup
}

type guildQueue struct {
	pending []*item
	current *item
	running bool // a worker goroutine owns this queue
	// connected is true while the bot holds a voice connection it opened itself.
	// It is cleared before the bot leaves on its own, so a later "left voice"
	// event can be told apart from being disconnected by someone else.
	connected bool
	kicked    bool          // disconnected by someone else; the queue was discarded
	idle      bool          // in voice with an empty queue, waiting (see Options.IdleTimeout)
	leaveNow  bool          // stop was asked for: don't stay idle, leave when the queue ends
	wake      chan struct{} // nudges an idle worker (new track, stop, kick, a guild waiting for a slot)
}

// nudge wakes g's worker if it is idle. Caller holds m.mu.
func (g *guildQueue) nudge() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

type item struct {
	Track
	done    chan struct{} // closed when Load finishes
	loaded  Loaded
	err     error
	cancel  context.CancelFunc // set while playing
	started time.Time          // when playback began; zero while loading (guarded by Manager.mu)
}

// Status is a read-only snapshot for the owner's debug command.
type Status struct {
	Slots     int // voice slots (servers in voice at once)
	SlotsUsed int
	Waiting   int // servers waiting for a voice slot
	Guilds    []GuildStatus
}

// GuildStatus is one server's queue.
type GuildStatus struct {
	GuildID   snowflake.ID
	Connected bool // in voice
	Idle      bool // in voice with nothing to play
	Current   *Track
	Playing   time.Duration // how far into Current; 0 while it loads
	Loading   bool          // Current is still downloading or being checked
	Queued    int
}

// Status reports every server with a queue, a current track or a voice
// connection, sorted by server ID.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Slots: cap(m.slot), SlotsUsed: len(m.active), Waiting: m.waiting}
	for id, g := range m.guilds {
		if g.current == nil && len(g.pending) == 0 && !g.connected {
			continue
		}
		gs := GuildStatus{GuildID: id, Connected: g.connected, Idle: g.idle, Queued: len(g.pending)}
		if it := g.current; it != nil {
			t := it.Track
			gs.Current = &t
			if it.started.IsZero() {
				gs.Loading = true
			} else {
				gs.Playing = time.Since(it.started)
			}
		}
		s.Guilds = append(s.Guilds, gs)
	}
	slices.SortFunc(s.Guilds, func(a, b GuildStatus) int { return cmp.Compare(a.GuildID, b.GuildID) })
	return s
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
		slot:      make(chan struct{}, max(opts.MaxSessions, 1)),
		idle:      max(opts.IdleTimeout, 0),
		after:     opts.After,
		guilds:    map[snowflake.ID]*guildQueue{},
		active:    map[snowflake.ID]bool{},
	}
	if m.after == nil {
		m.after = time.After
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

// Wait blocks until all playback and loading has stopped (after base is
// cancelled). From then on Enqueue refuses, so nothing starts after Wait has
// begun (sync.WaitGroup forbids that).
func (m *Manager) Wait() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) guild(id snowflake.ID) *guildQueue {
	g, ok := m.guilds[id]
	if !ok {
		g = &guildQueue{wake: make(chan struct{}, 1)}
		m.guilds[id] = g
	}
	return g
}

// Enqueue adds t to the guild's queue and starts loading it immediately.
func (m *Manager) Enqueue(guildID snowflake.ID, t Track) (Position, error) {
	m.mu.Lock()
	// Checked under the lock Wait takes, so a task is either counted before
	// Wait begins or never started.
	if m.closed {
		m.mu.Unlock()
		return Position{}, context.Canceled
	}
	if err := m.base.Err(); err != nil {
		m.mu.Unlock()
		return Position{}, err
	}
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
	g.leaveNow = false // a new request cancels an earlier stop
	g.nudge()
	pos := Position{Ahead: ahead, OtherGuild: !m.active[guildID] && m.busyOthers(guildID) >= cap(m.slot)}
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

// busyOthers counts other guilds holding a voice slot that aren't idle (idle
// ones give their slot up to a waiting guild). Caller holds m.mu.
func (m *Manager) busyOthers(guildID snowflake.ID) int {
	n := 0
	for id := range m.active {
		if id != guildID && !m.guilds[id].idle {
			n++
		}
	}
	return n
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
		if loaded.Title != "" && it.Title == "" { // a title given at enqueue (e.g. "Artist – Song" from Spotify) wins
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

	if !m.acquireSlot() {
		m.mu.Lock()
		g.running, g.pending = false, nil
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.active[guildID] = true
	m.mu.Unlock()
	defer func() {
		s.leave()
		<-m.slot
	}()

	idled := false // already stayed idle since the last track; next empty queue means leave
	for {
		m.mu.Lock()
		if g.kicked {
			// Discord already dropped us; closing just releases our side (DAVE session).
			g.kicked = false
			m.mu.Unlock()
			s.leave()
			m.mu.Lock()
		}
		if len(g.pending) == 0 && !idled && s.canIdle() {
			m.mu.Unlock()
			s.idleWait()
			idled = true
			continue
		}
		if len(g.pending) == 0 || m.base.Err() != nil {
			// Decided under the lock, so a concurrent Enqueue either lands before
			// this check or sees running=false and starts a new worker.
			g.running, g.pending, g.leaveNow = false, nil, false
			delete(m.active, guildID)
			m.mu.Unlock()
			return
		}
		idled = false
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

// acquireSlot waits for a free voice slot, first nudging idle guilds so one of
// them hands its slot over. It reports false on shutdown.
func (m *Manager) acquireSlot() bool {
	select {
	case m.slot <- struct{}{}:
		return true
	default:
	}
	m.mu.Lock()
	m.waiting++
	for id := range m.active {
		m.guilds[id].nudge()
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.waiting--
		m.mu.Unlock()
	}()
	select {
	case m.slot <- struct{}{}:
		return true
	case <-m.base.Done():
		return false
	}
}

// canIdle reports whether to stay in voice with an empty queue. Caller holds m.mu.
func (s *session) canIdle() bool {
	m := s.m
	return m.idle > 0 && s.conn != nil && m.base.Err() == nil && !s.g.leaveNow && !s.g.kicked && m.waiting == 0
}

// idleWait stays in voice until a track is queued, the idle timeout passes,
// stop is asked for, the bot is kicked, another guild needs the slot, or
// shutdown.
func (s *session) idleWait() {
	m, g := s.m, s.g
	timer := m.after(m.idle)
	s.log.Info("queue: idle in voice", "channel", s.channel, "timeout", m.idle)
	for {
		m.mu.Lock()
		if len(g.pending) > 0 || !s.canIdle() {
			g.idle = false
			m.mu.Unlock()
			return
		}
		g.idle = true
		m.mu.Unlock()

		select {
		case <-g.wake:
		case <-timer:
			m.mu.Lock()
			g.idle = false
			m.mu.Unlock()
			return
		case <-m.base.Done():
		}
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
	if loaded.OnStart != nil {
		loaded.OnStart()
	}
	m.mu.Lock()
	it.started = time.Now()
	m.mu.Unlock()
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
	g.nudge() // an idle worker leaves too
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
	if g == nil || (g.current == nil && len(g.pending) == 0 && !g.idle) {
		return false
	}
	g.pending, g.leaveNow = nil, true // no idle wait: stop means leave
	if g.current != nil {
		g.current.cancel()
	}
	g.nudge()
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

// InUse returns the keys of every queued or playing track, across all guilds.
// The cache uses it to never delete audio that is about to be played.
func (m *Manager) InUse() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := map[string]bool{}
	for _, g := range m.guilds {
		if g.current != nil && g.current.Key != "" {
			keys[g.current.Key] = true
		}
		for _, it := range g.pending {
			if it.Key != "" {
				keys[it.Key] = true
			}
		}
	}
	return keys
}
