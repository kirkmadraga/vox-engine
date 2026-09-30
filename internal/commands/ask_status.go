package commands

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// askCounts are ask's totals since the bot started (RAM only), for the
// owner's debug command. Counts only, never content.
type askCounts struct {
	answered, failed int
	refused          map[string]int // by reason, e.g. "cooldown"
	perUser          map[snowflake.ID]*userCounts
}

type userCounts struct{ answers, searches int }

// AskStatus is a read-only snapshot of ask for the owner's debug command.
type AskStatus struct {
	Search         string
	Cooldown       time.Duration
	MaxConcurrent  int
	MaxPromptChars int
	Timeout        time.Duration
	Answered       int
	Failed         int
	Refused        map[string]int
	SlotsBusy      int
	SlotsWaiting   int
	Users          []AskUser
}

// AskUser is one person's state: now, and since the bot started.
type AskUser struct {
	ID           snowflake.ID
	Running      bool
	Waiting      bool // a follow-up waits behind the running question
	CooldownLeft time.Duration
	Answers      int
	Searches     int
}

func (c *Ask) countRefused(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts.refused == nil {
		c.counts.refused = map[string]int{}
	}
	c.counts.refused[reason]++
}

func (c *Ask) countFailed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts.failed++
}

func (c *Ask) countAnswered(user snowflake.ID, searched bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts.answered++
	if c.counts.perUser == nil {
		c.counts.perUser = map[snowflake.ID]*userCounts{}
	}
	u := c.counts.perUser[user]
	if u == nil {
		u = &userCounts{}
		c.counts.perUser[user] = u
	}
	u.answers++
	if searched {
		u.searches++
	}
}

// failed counts a failure and tells the asker (privately where possible).
func (c *Ask) failed(ctx context.Context, req Request) error {
	c.countFailed()
	return privateReply(ctx, req, askFailed)
}

// Status reports ask's settings, totals and each person's state.
func (c *Ask) Status() AskStatus {
	if c.MaxConcurrent > 0 {
		c.once.Do(func() { c.slots = make(chan struct{}, c.MaxConcurrent) }) // as acquire does
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := AskStatus{
		Search: cmp.Or(c.Search, SearchOff), Cooldown: c.Cooldown, MaxConcurrent: c.MaxConcurrent,
		MaxPromptChars: c.MaxPromptChars, Timeout: c.Timeout,
		Answered: c.counts.answered, Failed: c.counts.failed, Refused: map[string]int{},
		SlotsBusy: len(c.slots), SlotsWaiting: int(c.slotWaiting.Load()),
	}
	for k, v := range c.counts.refused {
		s.Refused[k] = v
	}
	now := c.now()
	seen := map[snowflake.ID]bool{}
	add := func(id snowflake.ID) {
		if seen[id] {
			return
		}
		seen[id] = true
		u := AskUser{ID: id}
		if a := c.users[id]; a != nil {
			u.Running, u.Waiting = a.running, a.waiting
			if !c.owner(id) {
				u.CooldownLeft = max(a.next.Sub(now), 0)
			}
		}
		if n := c.counts.perUser[id]; n != nil {
			u.Answers, u.Searches = n.answers, n.searches
		}
		s.Users = append(s.Users, u)
	}
	for id := range c.users {
		add(id)
	}
	for id := range c.counts.perUser {
		add(id)
	}
	slices.SortFunc(s.Users, func(a, b AskUser) int { return cmp.Compare(a.ID, b.ID) })
	return s
}
