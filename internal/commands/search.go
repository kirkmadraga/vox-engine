package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// DefaultRecentTTL is how long a search's results can be picked with "play <number>".
const DefaultRecentTTL = 5 * time.Minute

// RecentSearches remembers each user's last search results per server, in
// memory only, so "play <number>" can pick one. Entries expire after the TTL.
type RecentSearches struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[recentKey]recentEntry
}

type recentKey struct{ guild, user snowflake.ID }

type recentEntry struct {
	hits []ytdlp.Hit
	at   time.Time
}

// NewRecentSearches returns an empty store. ttl 0 = DefaultRecentTTL; now nil = time.Now.
func NewRecentSearches(ttl time.Duration, now func() time.Time) *RecentSearches {
	if ttl <= 0 {
		ttl = DefaultRecentTTL
	}
	if now == nil {
		now = time.Now
	}
	return &RecentSearches{ttl: ttl, now: now, m: make(map[recentKey]recentEntry)}
}

// Put replaces the user's results in guild, and drops expired entries.
func (r *RecentSearches) Put(guild, user snowflake.ID, hits []ytdlp.Hit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, e := range r.m {
		if now.Sub(e.at) >= r.ttl {
			delete(r.m, k)
		}
	}
	r.m[recentKey{guild, user}] = recentEntry{hits: append([]ytdlp.Hit(nil), hits...), at: now}
}

// Get returns the user's unexpired results in guild.
func (r *RecentSearches) Get(guild, user snowflake.ID) ([]ytdlp.Hit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[recentKey{guild, user}]
	if !ok || r.now().Sub(e.at) >= r.ttl {
		return nil, false
	}
	return append([]ytdlp.Hit(nil), e.hits...), true
}

// formatResults lists search results for "play <number>".
func formatResults(hits []ytdlp.Hit) string {
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. ", i+1)
		if h.Title != "" {
			b.WriteString("**" + queue.EscapeMarkdown(h.Title) + "** ")
		}
		if h.Duration > 0 {
			b.WriteString("(" + queue.FormatDuration(h.Duration) + ") ")
		}
		b.WriteString("<https://youtu.be/" + h.ID + ">\n")
	}
	b.WriteString("To pick one, play its number.")
	return truncate(b.String(), maxMessageLen)
}

// searchFailed replies to a failed search. Unexpected failures are also
// returned, so the router logs them.
func searchFailed(ctx context.Context, req Request, err error) error {
	var yerr *ytdlp.Error
	switch {
	case errors.Is(err, ytdlp.ErrNoResults):
		return reply(ctx, req, "No YouTube results for that.")
	case errors.As(err, &yerr) && (yerr.Kind == ytdlp.KindBotCheck || yerr.Kind == ytdlp.KindMissingTool):
		return errors.Join(reply(ctx, req, "Couldn't search: %s", yerr.UserMessage()), fmt.Errorf("search: %w", err))
	default:
		return errors.Join(reply(ctx, req, "The YouTube search failed. Details are in the bot's log."), fmt.Errorf("search: %w", err))
	}
}
