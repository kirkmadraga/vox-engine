package commands

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// AskCommand is the name of the LLM command. A mention whose first word isn't a
// command is routed to it too ("@Bot what's jazz?").
const AskCommand = "ask"

// Ask is "@Bot ask <prompt>" (or "@Bot <prompt>", or /ask): it sends the prompt
// to the configured model and replies with the answer. Prompts and answers are
// never logged.
//
// Load limits (the bot operator's choices):
//   - Questions longer than MaxPromptChars are refused.
//   - The daily allowance (Daily) must not be used up; see DailyLimit.
//   - Each user has one question running and at most one more waiting behind
//     it (a quick follow-up); a third gets an error at once.
//   - Cooldown: a user's questions start at least Cooldown apart, counted from
//     when the previous one started. A new question that's too early gets an
//     error; a waiting follow-up waits it out. Owners are exempt.
//   - At most MaxConcurrent questions reach the model at once, owners
//     included; the rest wait their turn silently (no error for waiting).
//   - Timeout bounds waiting plus answering; past it, the question fails.
//   - An answer shows at most maxAnswerParts Discord messages.
//
// Only questions that reach the model start the cooldown or count against the
// daily allowance; refusals and hints cost nothing. Errors are private on
// slash commands.
type Ask struct {
	LLM            llm.Provider
	Logger         *slog.Logger
	Cooldown       time.Duration // 0 = none
	MaxConcurrent  int           // 0 = no limit
	MaxPromptChars int           // 0 = no limit
	Timeout        time.Duration // per question, waiting included; 0 = none
	Daily          *DailyLimit   // nil = no daily limit
	// Search is when web search is allowed: SearchOff, SearchOnRequest (the
	// question starts with "search", or /ask search:True) or SearchAlways.
	Search  string
	IsOwner func(snowflake.ID) bool                    // exempt from the cooldown; nil = nobody
	Memory  *ChannelMemory                             // the channel's recent conversation; nil = none
	Answers *AnswerLog                                 // records answers, so replies to them continue; nil = none
	Now     func() time.Time                           // for tests; nil = time.Now
	Sleep   func(context.Context, time.Duration) error // for tests; nil = a real timer

	once        sync.Once
	slots       chan struct{}
	slotWaiting atomic.Int32 // questions waiting for a free slot

	mu     sync.Mutex
	users  map[snowflake.ID]*asker
	counts askCounts // totals since start, for debug
}

// asker is one user's state.
type asker struct {
	running bool
	waiting bool
	done    chan struct{} // closed when the running question finishes
	next    time.Time     // earliest start of the user's next question
}

func (*Ask) Name() string { return AskCommand }

// When ask allows web search (Ask.Search). Searching costs several times a
// plain answer (search results are fed back to the model), so by default
// only questions that ask for it may search.
const (
	SearchOff       = "off"
	SearchOnRequest = "on-request"
	SearchAlways    = "always"
)

// SearchWord at the start of a question asks for a web search.
const SearchWord = "search"

// SearchMark starts a question sent to the model when a search was requested
// (with SearchOnRequest), so the system prompt can say "a question marked
// [search requested] may search". Memory keeps the question without it.
const SearchMark = "[search requested]"

// wantsSearch strips a leading "search" from prompt and reports whether it was there.
func wantsSearch(prompt string) (string, bool) {
	word, rest, _ := strings.Cut(prompt, " ")
	if !strings.EqualFold(word, SearchWord) || strings.TrimSpace(rest) == "" {
		return prompt, false
	}
	return strings.TrimSpace(rest), true
}

// maxAnswerParts caps an answer's Discord messages, whatever the model does.
const maxAnswerParts = 3

const (
	askUsage      = "Ask me something, e.g. `ask what's a good road trip song?`"
	askQueueFull  = "You already have a question waiting. I'll answer it right after this one."
	askFailed     = "I couldn't get an answer right now. Details are in the bot's log."
	askEmpty      = "I didn't get an answer for that one."
	askDailyLimit = "You've hit your daily ask limit for today."
	// An image question costs more; said when the rest of the allowance can't cover it.
	askDailyLimitImage = "You don't have enough of today's ask limit left for a question with an image."
	// A search costs more still; said when the rest of the allowance can't cover it.
	askDailyLimitSearch = "You don't have enough of today's ask limit left for a search. Ask without \"search\" to get an answer without one."
	askCutShort         = "\n…(answer cut short)"
)

func askCooldownMessage(wait time.Duration) string {
	secs := int((wait + time.Second - 1) / time.Second) // round up: never "0s"
	return fmt.Sprintf("You can ask again in %ds.", secs)
}

func askTooLongMessage(limit int) string {
	return fmt.Sprintf("That's too long, keep it under %d characters.", limit)
}

func askQuoteTooLongMessage(limit int) string {
	return fmt.Sprintf("The message you replied to is too long for me to take in with your question (over %d characters together).", limit)
}

// quote returns the replied-to message to quote to the model, or nil: none,
// or one still in the channel's memory (a recent question or answer), which
// the model gets anyway. One of the bot's answers that memory has forgotten
// (older than the memory window, or from before a restart) is quoted, so a
// reply to it keeps its context.
func (c *Ask) quote(req Request) *Quote {
	q := req.Quoted
	if q == nil || c.Memory.Has(req.ChannelID, q.MessageID) {
		return nil
	}
	return q
}

// maxSelfQuote caps how much of one of the bot's own answers is quoted: its
// end, which a reply most likely follows up on.
const maxSelfQuote = 500

// withQuote puts the replied-to message in front of the question.
func withQuote(prompt string, q *Quote) string {
	if q == nil {
		return prompt
	}
	quoted := q.Content
	if r := []rune(quoted); q.Self && len(r) > maxSelfQuote {
		quoted = "…" + string(r[len(r)-maxSelfQuote:])
	}
	if len(q.Images) > 0 {
		quoted = strings.TrimSpace(quoted + " [image]")
	}
	return fmt.Sprintf("[Replying to %s: %q]\n%s", q.AuthorName, quoted, prompt)
}

// pickImage chooses the one image a question may carry: the asker's first,
// else the replied-to message's first. Others are ignored.
func pickImage(own []Image, q *Quote) (*Image, string) {
	if len(own) > 0 {
		return &own[0], "asker"
	}
	if q != nil && len(q.Images) > 0 {
		return &q.Images[0], "replied-to message"
	}
	return nil, ""
}

func (c *Ask) Run(ctx context.Context, req Request) error {
	prompt := strings.TrimSpace(req.Args)
	user := req.AuthorID
	log := c.logger().With("user", user, "guild", req.GuildID, "provider", c.LLM.Name())
	refuse := func(reason, content string, attrs ...any) error {
		c.countRefused(reason)
		log.Debug("ask: refused", append([]any{"reason", reason}, attrs...)...)
		return privateReply(ctx, req, content)
	}
	if prompt == "" {
		return refuse("empty", askUsage)
	}
	search := c.Search == SearchAlways
	requested := false // on request, and asked for: the model is told so
	if c.Search == SearchOnRequest || c.Search == SearchAlways {
		var asked bool
		prompt, asked = wantsSearch(prompt)
		asked = asked || req.Search
		search = search || asked
		if asked && c.Search == SearchOnRequest {
			requested = true
			log.Debug("ask: search requested")
		}
	}
	quote := c.quote(req)
	n := utf8.RuneCountInString(prompt)
	if quote != nil && !quote.Self {
		// Someone's replied-to text counts too; the bot's own answer is trimmed instead.
		n += utf8.RuneCountInString(quote.Content)
	}
	if c.MaxPromptChars > 0 && n > c.MaxPromptChars {
		if quote != nil && quote.Content != "" {
			return refuse("too long with the replied-to message", askQuoteTooLongMessage(c.MaxPromptChars), "prompt_len", n)
		}
		return refuse("too long", askTooLongMessage(c.MaxPromptChars), "prompt_len", n)
	}
	image, from := pickImage(req.Images, quote)
	if image != nil {
		log.Debug("ask: image", "from", from, "size", image.Size, "type", image.ContentType)
	}
	if reason, msg, attrs, err := c.affordable(ctx, user, requested, image != nil); err != nil {
		log.Error("ask: couldn't read the daily balance", "err", err)
		return c.failed(ctx, req)
	} else if reason != "" {
		return refuse(reason, msg, attrs...)
	}
	mine, prev, refusal := c.admit(user)
	switch refusal {
	case "":
	case askQueueFull:
		return refuse("queue full", refusal)
	default:
		return refuse("cooldown", refusal)
	}
	if t, ok := req.Reply.(TypingIndicator); ok {
		stop := t.StartTyping() // covers waiting too; stopped after the answer is sent
		defer stop()
	}

	// qctx bounds waiting and answering; replies use ctx, so a timed-out
	// question can still be told so.
	qctx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		qctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	arrived := c.now()
	if prev != nil {
		// A follow-up: wait for the user's running question, then its cooldown.
		log.Debug("ask: follow-up waiting behind the user's previous question")
		select {
		case <-prev:
		case <-qctx.Done():
			c.abandon(user)
			log.Warn("ask: gave up waiting behind the user's previous question", "err", qctx.Err())
			return c.failed(ctx, req)
		}
		var wait time.Duration
		mine, wait = c.promote(user)
		defer c.finish(user, mine)
		if wait > 0 {
			log.Debug("ask: follow-up waiting for the cooldown", "wait", wait)
		}
		if err := c.sleep(qctx, wait); err != nil {
			log.Warn("ask: gave up waiting for the cooldown", "err", err)
			return c.failed(ctx, req)
		}
		// The first answer may have used up the allowance meanwhile.
		if reason, msg, attrs, err := c.affordable(ctx, user, requested, image != nil); err != nil {
			log.Error("ask: couldn't read the daily balance", "err", err)
			return c.failed(ctx, req)
		} else if reason != "" {
			return refuse(reason, msg, append(attrs, "follow_up", true)...)
		}
	} else {
		defer c.finish(user, mine)
	}
	if err := c.acquire(qctx); err != nil {
		log.Warn("ask: gave up waiting for a free slot", "err", err)
		return c.failed(ctx, req)
	}
	// Read memory only now: a follow-up then sees the answer it followed.
	question := llm.Message{Role: llm.User, Name: req.AuthorName, Content: withQuote(prompt, quote)}
	remembered := question // memory keeps "[image]" as text, never the image
	if image != nil {
		question.Image = &llm.Image{URL: image.URL, ContentType: image.ContentType}
		remembered.Content += " [image]"
	}
	if requested {
		// The bot decides whether search is allowed and says so, instead of
		// leaving the model to look for the word (which was removed).
		question.Content = SearchMark + " " + question.Content
	}
	conversation := append(c.Memory.History(req.ChannelID), question)
	began := c.now()
	log.Debug("ask: sending", "waited", began.Sub(arrived).Round(time.Millisecond), "history", len(conversation)-1)
	c.started(user, began)
	answer, err := c.LLM.Complete(qctx, llm.Request{Conversation: conversation, Search: search})
	c.release()
	took := c.now().Sub(began).Round(time.Millisecond)
	if err != nil {
		log.Error("ask: failed", "err", err, "took", took, "prompt_len", n, "timed_out", qctx.Err() != nil)
		return c.failed(ctx, req)
	}
	if image != nil {
		answer.Usage.ViewedImages = true // the model was given an image to look at
	}
	parts, cut := capAnswer(splitMessage(answer.Text, maxMessageLen))
	var balanceAttrs []any
	if balance, limited, err := c.Daily.Charge(ctx, user, answer.Usage); err != nil {
		log.Error("ask: couldn't update the daily balance", "err", err)
	} else if limited {
		balanceAttrs = []any{"weight", c.Daily.Weight(answer.Usage), "balance", balance}
	}
	seq := 0
	if len(parts) > 0 {
		shown := strings.Join(parts, "\n") // remember what people saw
		seq = c.Memory.Add(req.ChannelID, req.MessageID, remembered, llm.Message{Role: llm.Assistant, Content: shown})
	}
	c.countAnswered(user, answer.Usage.WebSearch)
	c.finish(user, mine) // free the user's follow-up now (after remembering); the deferred call is then a no-op
	log.Info("ask: answered", append([]any{"took", took, "prompt_len", n, "answer_len", utf8.RuneCountInString(answer.Text),
		"parts", len(parts), "cut", cut, "history", len(conversation) - 1,
		"search", answer.Usage.WebSearch, "images", answer.Usage.ViewedImages}, balanceAttrs...)...)
	if len(parts) == 0 {
		return reply(ctx, req, askEmpty)
	}
	for i, p := range parts {
		// No Mentions: the model's text can't ping anyone. The first part
		// replies to the question, so it's clear whose answer it is.
		r := Reply{Content: p, Sent: func(id snowflake.ID) {
			c.Answers.Add(req.ChannelID, id)
			c.Memory.AddAnswerMessage(req.ChannelID, seq, id)
		}}
		if i == 0 {
			r.ReplyTo, r.PingReplied = req.MessageID, true // notifies the asker, once
		}
		err := req.Reply.Reply(ctx, r)
		if err != nil && i > 0 && ctx.Err() == nil {
			// Later parts get one more try here. The first part isn't retried
			// here: on mentions the replier re-sends it as a plain message;
			// on /ask it isn't re-sent, as a second try would become a
			// follow-up and leave "thinking…" showing. The operator's choice:
			// a rare double post beats an answer cut short after it was paid for.
			log.Warn("ask: re-sending a part", "part", i+1, "err", err)
			err = req.Reply.Reply(ctx, r)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// affordable checks user's daily allowance before a question goes ahead: the
// balance must be above 0, and must cover a requested search's and an image's
// cost, which are known up front. (With SearchAlways the model decides, so
// only what it did is charged.) reason is "" when the question may go ahead;
// otherwise it names the refusal, msg is what to tell the user and attrs are
// for the log.
func (c *Ask) affordable(ctx context.Context, user snowflake.ID, searchRequested, image bool) (reason, msg string, attrs []any, err error) {
	ok, balance, err := c.dailyOK(ctx, user)
	switch {
	case err != nil:
		return "", "", nil, err
	case !ok:
		return "daily limit", askDailyLimit, []any{"balance", balance}, nil
	case !c.Daily.limits(user):
		return "", "", nil, nil
	}
	need := c.Daily.Weight(llm.Usage{WebSearch: searchRequested, ViewedImages: image})
	switch {
	case balance >= need:
		return "", "", nil, nil
	case searchRequested:
		return "daily limit (search)", askDailyLimitSearch, []any{"balance", balance, "need", need}, nil
	default:
		return "daily limit (image)", askDailyLimitImage, []any{"balance", balance, "need", need}, nil
	}
}

// dailyOK reports whether user may ask now under the daily allowance (always,
// when they aren't limited), with their balance for logs.
func (c *Ask) dailyOK(ctx context.Context, user snowflake.ID) (ok bool, balance int, err error) {
	balance, limited, err := c.Daily.Balance(ctx, user)
	if err != nil || !limited {
		return err == nil, balance, err
	}
	return balance > 0, balance, nil
}

// capAnswer keeps at most maxAnswerParts parts, marking a cut on the last one
// (still within Discord's limit).
func capAnswer(parts []string) (kept []string, cut bool) {
	if len(parts) <= maxAnswerParts {
		return parts, false
	}
	kept = slices.Clone(parts[:maxAnswerParts])
	last := []rune(kept[maxAnswerParts-1])
	if room := maxMessageLen - utf8.RuneCountInString(askCutShort); len(last) > room {
		last = last[:room]
	}
	kept[maxAnswerParts-1] = string(last) + askCutShort
	return kept, true
}

// admit decides whether user's new question may go ahead. mine identifies it
// once it runs; prev is non-nil when it's a follow-up that must wait for the
// running one (closed when that finishes); refusal is non-empty when it may not.
func (c *Ask) admit(user snowflake.ID) (mine chan struct{}, prev <-chan struct{}, refusal string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.users == nil {
		c.users = make(map[snowflake.ID]*asker)
	}
	u := c.users[user]
	if u == nil {
		u = &asker{}
		c.users[user] = u
	}
	switch {
	case u.waiting: // a follow-up is queued (its predecessor may have just finished)
		return nil, nil, askQueueFull
	case u.running:
		u.waiting = true
		return nil, u.done, ""
	}
	if wait := u.next.Sub(c.now()); wait > 0 && !c.owner(user) {
		return nil, nil, askCooldownMessage(wait)
	}
	u.running, u.done = true, make(chan struct{})
	return u.done, nil, ""
}

// promote turns user's waiting follow-up into the running question (mine)
// and returns how long it must still wait for the cooldown.
func (c *Ask) promote(user snowflake.ID) (mine chan struct{}, wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.users[user]
	u.waiting, u.running, u.done = false, true, make(chan struct{})
	if c.owner(user) {
		return u.done, 0
	}
	return u.done, max(u.next.Sub(c.now()), 0)
}

// abandon drops user's waiting follow-up, which gave up before its turn.
func (c *Ask) abandon(user snowflake.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users[user].waiting = false
}

// started records that user's question reached the model at t.
func (c *Ask) started(user snowflake.ID, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users[user].next = t.Add(c.Cooldown)
}

// finish ends user's running question mine and lets a waiting follow-up go.
// It does nothing if mine already finished (it may be called twice), so it
// never ends the follow-up that took over.
func (c *Ask) finish(user snowflake.ID, mine chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.users[user]
	if u == nil || !u.running || u.done != mine {
		return
	}
	u.running = false
	close(u.done)
}

func (c *Ask) acquire(ctx context.Context) error {
	if c.MaxConcurrent <= 0 {
		return nil
	}
	c.once.Do(func() { c.slots = make(chan struct{}, c.MaxConcurrent) })
	c.slotWaiting.Add(1)
	defer c.slotWaiting.Add(-1)
	select {
	case c.slots <- struct{}{}: // blocked senders are served in arrival order
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Ask) release() {
	if c.MaxConcurrent > 0 {
		<-c.slots
	}
}

func (c *Ask) owner(user snowflake.ID) bool { return c.IsOwner != nil && c.IsOwner(user) }

func (c *Ask) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Ask) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Ask) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// splitMessage cuts s into parts of at most limit characters, preferring to
// break at a line break, then at a space. Parts are trimmed; empty ones are dropped.
func splitMessage(s string, limit int) []string {
	var parts []string
	rest := []rune(strings.TrimSpace(s))
	for len(rest) > 0 {
		if len(rest) <= limit {
			parts = append(parts, string(rest))
			break
		}
		cut := lastIndex(rest[:limit+1], '\n')
		if cut <= 0 {
			cut = lastIndex(rest[:limit+1], ' ')
		}
		if cut <= 0 {
			cut = limit
		}
		if p := strings.TrimSpace(string(rest[:cut])); p != "" {
			parts = append(parts, p)
		}
		rest = []rune(strings.TrimSpace(string(rest[cut:])))
	}
	return parts
}

func lastIndex(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}
