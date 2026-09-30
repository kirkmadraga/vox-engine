package discordio

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// TypingSender is the subset of disgo's REST client that shows "typing…".
type TypingSender interface {
	SendTyping(channelID snowflake.ID, opts ...rest.RequestOpt) error
}

// typingInterval re-sends "typing…" before Discord drops it (after ~10 s).
const typingInterval = 8 * time.Second

// ChannelTyping shows "Bot is typing…" in a channel while any command there
// wants it: one indicator per channel however many are waiting. Discord also
// clears it when the bot posts, so when one command stops while others in the
// channel still wait (typically right after posting its answer), it's sent
// again at once.
type ChannelTyping struct {
	Sender   TypingSender
	Logger   *slog.Logger
	Interval time.Duration // 0 = typingInterval

	mu       sync.Mutex
	channels map[snowflake.ID]*typingChannel
}

type typingChannel struct {
	users int
	quit  chan struct{}
}

// Start begins (or joins) the indicator in channelID. Each Start needs one
// call of the returned stop; extra calls are ignored.
func (t *ChannelTyping) Start(channelID snowflake.ID) (stop func()) {
	t.mu.Lock()
	if t.channels == nil {
		t.channels = make(map[snowflake.ID]*typingChannel)
	}
	ch := t.channels[channelID]
	if ch == nil {
		ch = &typingChannel{quit: make(chan struct{})}
		t.channels[channelID] = ch
		go t.loop(channelID, ch.quit)
	}
	ch.users++
	t.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { t.stop(channelID, ch) }) }
}

func (t *ChannelTyping) stop(channelID snowflake.ID, ch *typingChannel) {
	t.mu.Lock()
	ch.users--
	last := ch.users == 0
	if last {
		close(ch.quit)
		delete(t.channels, channelID)
	}
	t.mu.Unlock()
	if !last {
		t.send(channelID) // someone still waits: bring it back after the answer cleared it
	}
}

func (t *ChannelTyping) loop(channelID snowflake.ID, quit <-chan struct{}) {
	interval := t.Interval
	if interval <= 0 {
		interval = typingInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	t.send(channelID)
	for {
		select {
		case <-tick.C:
			t.send(channelID)
		case <-quit:
			return
		}
	}
}

func (t *ChannelTyping) send(channelID snowflake.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	if err := t.Sender.SendTyping(channelID, rest.WithCtx(ctx)); err != nil && t.Logger != nil {
		t.Logger.Debug("typing indicator failed", "channel", channelID, "err", err)
	}
}
