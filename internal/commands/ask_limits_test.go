package commands

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// Tests for ask's load limits: the cooldown, one waiting follow-up per user,
// and the shared limit on questions reaching the model. The limits are the
// bot operator's choices; see the Ask doc comment. A refusal must never reach
// the model: "no call to the fake" means "no tokens spent".

// gateLLM answers "answer to <prompt>". Prompts listed in gates wait until
// their gate is closed; every call reports its prompt on started.
type gateLLM struct {
	started chan string

	mu          sync.Mutex
	gates       map[string]chan struct{}
	got         []string
	inFlight    int
	maxInFlight int
}

func newGateLLM(held ...string) *gateLLM {
	g := &gateLLM{started: make(chan string, 100), gates: map[string]chan struct{}{}}
	for _, p := range held {
		g.gates[p] = make(chan struct{})
	}
	return g
}

func (g *gateLLM) Name() string { return "gate" }

func (g *gateLLM) Complete(ctx context.Context, req llm.Request) (llm.Answer, error) {
	conv := req.Conversation
	prompt := conv[len(conv)-1].Content
	g.mu.Lock()
	g.got = append(g.got, prompt)
	g.inFlight++
	g.maxInFlight = max(g.maxInFlight, g.inFlight)
	gate := g.gates[prompt]
	g.mu.Unlock()
	g.started <- prompt
	var err error
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	if err != nil {
		return llm.Answer{}, err
	}
	return llm.Answer{Text: "answer to " + prompt}, nil
}

func (g *gateLLM) release(prompt string) { close(g.gates[prompt]) }

func (g *gateLLM) prompts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.got)
}

// clock is a fake clock. Sleeping advances it at once and records the wait.
type clock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *clock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

const askOwner snowflake.ID = 1

func newLimitedAsk(llm *gateLLM, clk *clock) *Ask {
	return &Ask{
		LLM: llm, Cooldown: 10 * time.Second, MaxConcurrent: 3,
		IsOwner: func(id snowflake.ID) bool { return id == askOwner },
		Now:     clk.now, Sleep: clk.sleep,
	}
}

// ask runs one question from user and returns its replies.
func ask(t *testing.T, c *Ask, user snowflake.ID, prompt string) []Reply {
	t.Helper()
	rep := &syncReplier{}
	if err := c.Run(context.Background(), Request{AuthorID: user, Args: prompt, Reply: rep}); err != nil {
		t.Fatal(err)
	}
	return rep.replies()
}

// askAsync runs a question in the background; the replies arrive on the channel.
func askAsync(ctx context.Context, c *Ask, user snowflake.ID, prompt string) <-chan []Reply {
	out := make(chan []Reply, 1)
	go func() {
		rep := &syncReplier{}
		c.Run(ctx, Request{AuthorID: user, Args: prompt, Reply: rep})
		out <- rep.replies()
	}()
	return out
}

type syncReplier struct {
	mu  sync.Mutex
	got []Reply
}

func (s *syncReplier) Reply(_ context.Context, r Reply) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
	return nil
}

func (s *syncReplier) replies() []Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// only asserts replies is exactly one reply with content, private or not.
func only(t *testing.T, what string, replies []Reply, content string, private bool) {
	t.Helper()
	if len(replies) != 1 || replies[0].Content != content || replies[0].Private != private {
		t.Errorf("%s: replies = %+v, want one %q (private %v)", what, replies, content, private)
	}
}

// waitUntil polls cond (the tests' only way to see that a goroutine is parked).
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

func (c *Ask) isWaiting(user snowflake.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.users[user]
	return u != nil && u.waiting
}

func TestAskCooldownRefusesWithoutAskingTheModel(t *testing.T) {
	llm, clk := newGateLLM(), newClock()
	c := newLimitedAsk(llm, clk)
	only(t, "first", ask(t, c, 8, "one"), "answer to one", false)
	only(t, "at once", ask(t, c, 8, "two"), "You can ask again in 10s.", true)
	clk.advance(4 * time.Second)
	only(t, "4s later", ask(t, c, 8, "two"), "You can ask again in 6s.", true)
	clk.advance(1500 * time.Millisecond)
	only(t, "5.5s later", ask(t, c, 8, "two"), "You can ask again in 5s.", true) // rounds up
	clk.advance(4500 * time.Millisecond)
	only(t, "10s later", ask(t, c, 8, "two"), "answer to two", false)
	if got := llm.prompts(); !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("the model got %q; refusals must not reach it", got)
	}
}

func TestAskCooldownIsPerUser(t *testing.T) {
	c := newLimitedAsk(newGateLLM(), newClock())
	only(t, "user 8", ask(t, c, 8, "one"), "answer to one", false)
	only(t, "user 9 right after", ask(t, c, 9, "two"), "answer to two", false)
}

func TestAskOwnersSkipTheCooldown(t *testing.T) {
	c := newLimitedAsk(newGateLLM(), newClock())
	only(t, "first", ask(t, c, askOwner, "one"), "answer to one", false)
	only(t, "at once", ask(t, c, askOwner, "two"), "answer to two", false)
}

func TestAskFreeRepliesDontStartTheCooldown(t *testing.T) {
	llm, clk := newGateLLM(), newClock()
	c := newLimitedAsk(llm, clk)
	only(t, "usage", ask(t, c, 8, "  "), askUsage, true)
	only(t, "right after the usage hint", ask(t, c, 8, "one"), "answer to one", false)
	clk.advance(5 * time.Second)
	ask(t, c, 8, "early") // refused: must not restart the 10s
	clk.advance(5 * time.Second)
	only(t, "10s after the first", ask(t, c, 8, "two"), "answer to two", false)
}

// A second question while the first runs waits (silently) and runs right
// after, once the cooldown since the first question's start is over; a third
// is refused at once.
func TestAskOneFollowUpWaitsAThirdIsRefused(t *testing.T) {
	llm, clk := newGateLLM("one"), newClock()
	c := newLimitedAsk(llm, clk)
	first := askAsync(context.Background(), c, 8, "one")
	<-llm.started
	clk.advance(3 * time.Second) // the first answer takes 3s
	second := askAsync(context.Background(), c, 8, "two")
	waitUntil(t, "the follow-up is waiting", func() bool { return c.isWaiting(8) })

	only(t, "third", ask(t, c, 8, "three"), askQueueFull, true)
	select {
	case r := <-second:
		t.Fatalf("the follow-up must wait silently, got %+v", r)
	default:
	}

	llm.release("one")
	only(t, "first", <-first, "answer to one", false)
	only(t, "follow-up", <-second, "answer to two", false)
	if got := llm.prompts(); !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("the model got %q", got)
	}
	if !slices.Equal(clk.sleeps, []time.Duration{7 * time.Second}) {
		t.Errorf("the follow-up waited %v, want the 7s left of the cooldown", clk.sleeps)
	}
	// After both, the queue is free again (the cooldown still applies).
	clk.advance(10 * time.Second)
	only(t, "later", ask(t, c, 8, "four"), "answer to four", false)
}

func TestAskOwnersStillGetOnlyOneFollowUp(t *testing.T) {
	llm, clk := newGateLLM("one"), newClock()
	c := newLimitedAsk(llm, clk)
	first := askAsync(context.Background(), c, askOwner, "one")
	<-llm.started
	second := askAsync(context.Background(), c, askOwner, "two")
	waitUntil(t, "the follow-up is waiting", func() bool { return c.isWaiting(askOwner) })
	only(t, "third", ask(t, c, askOwner, "three"), askQueueFull, true)
	llm.release("one")
	<-first
	only(t, "follow-up", <-second, "answer to two", false)
	if len(clk.sleeps) != 0 {
		t.Errorf("owners skip the cooldown, but the follow-up slept %v", clk.sleeps)
	}
}

// With every slot busy, more questions wait their turn (no error), owners
// included, and never more than MaxConcurrent reach the model at once.
func TestAskMaxConcurrentQueuesTheRest(t *testing.T) {
	llm := newGateLLM("a", "b", "c", "d")
	c := newLimitedAsk(llm, newClock())
	c.MaxConcurrent = 2
	a := askAsync(context.Background(), c, 11, "a")
	b := askAsync(context.Background(), c, 12, "b")
	started := map[string]bool{<-llm.started: true, <-llm.started: true}
	if !started["a"] || !started["b"] {
		t.Fatalf("started %v", started)
	}
	cq := askAsync(context.Background(), c, askOwner, "c") // owners wait too
	select {
	case p := <-llm.started:
		t.Fatalf("%q reached the model while both slots were busy", p)
	case <-time.After(50 * time.Millisecond):
	}

	llm.release("a")
	only(t, "a", <-a, "answer to a", false)
	if p := <-llm.started; p != "c" {
		t.Fatalf("started %q, want c", p)
	}
	llm.release("b")
	llm.release("c")
	only(t, "b", <-b, "answer to b", false)
	only(t, "c", <-cq, "answer to c", false)
	if llm.maxInFlight > 2 {
		t.Errorf("%d questions reached the model at once, limit 2", llm.maxInFlight)
	}
}

// Waiting too long for a slot gives the friendly error, and the slot isn't lost.
func TestAskGivesUpWaitingForASlot(t *testing.T) {
	llm := newGateLLM("a")
	c := newLimitedAsk(llm, newClock())
	c.MaxConcurrent = 1
	a := askAsync(context.Background(), c, 11, "a")
	<-llm.started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	only(t, "timed out", <-askAsync(ctx, c, 12, "b"), askFailed, true)
	llm.release("a")
	<-a
	only(t, "the slot is free again", ask(t, c, 13, "c"), "answer to c", false)
	if slices.Contains(llm.prompts(), "b") {
		t.Error("a question that gave up must not reach the model")
	}
}

// A follow-up that gives up waiting frees the queue without ending the
// running question.
func TestAskWaitingFollowUpGivesUp(t *testing.T) {
	llm, clk := newGateLLM("one"), newClock()
	c := newLimitedAsk(llm, clk)
	first := askAsync(context.Background(), c, 8, "one")
	<-llm.started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	only(t, "gave up", <-askAsync(ctx, c, 8, "two"), askFailed, true)

	third := askAsync(context.Background(), c, 8, "three") // queues again: the slot was freed
	waitUntil(t, "the new follow-up is waiting", func() bool { return c.isWaiting(8) })
	llm.release("one")
	only(t, "first", <-first, "answer to one", false)
	only(t, "new follow-up", <-third, "answer to three", false)
}

// typingReplier records the order of typing starts/stops and replies.
type typingReplier struct {
	syncReplier
	events []string
}

func (r *typingReplier) Reply(ctx context.Context, rep Reply) error {
	r.mu.Lock()
	r.events = append(r.events, "reply")
	r.mu.Unlock()
	return r.syncReplier.Reply(ctx, rep)
}

func (r *typingReplier) StartTyping() func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "start")
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, "stop")
	}
}

func TestAskShowsTypingUntilTheAnswerIsSent(t *testing.T) {
	c := newLimitedAsk(newGateLLM(), newClock())
	rep := &typingReplier{}
	c.Run(context.Background(), Request{AuthorID: 8, Args: "one", Reply: rep})
	if !slices.Equal(rep.events, []string{"start", "reply", "stop"}) {
		t.Errorf("events %q", rep.events)
	}
	rep.events = nil
	c.Run(context.Background(), Request{AuthorID: 8, Args: "two", Reply: rep}) // cooldown
	if !slices.Equal(rep.events, []string{"reply"}) {
		t.Errorf("a refusal must not show typing: events %q", rep.events)
	}
}
