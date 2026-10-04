package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// Guardrails in front of the model: question length, the daily allowance,
// the per-question timeout and the answer length. Refusals never reach the
// model, which the fakes' call records prove.

func TestAskQuestionLengthCap(t *testing.T) {
	f := &fakeLLM{answer: "ok"}
	c := &Ask{LLM: f, MaxPromptChars: 500}
	only(t, "500 characters", ask(t, c, 8, strings.Repeat("a", 500)), "ok", false)
	only(t, "501 characters", ask(t, c, 9, strings.Repeat("a", 501)), "That's too long, keep it under 500 characters.", true)
	only(t, "counted in characters, not bytes", ask(t, c, 10, strings.Repeat("ñ", 500)), "ok", false)
	if len(f.got) != 2 {
		t.Errorf("the model got %d questions, want 2 (the long one refused)", len(f.got))
	}
}

func TestAskDailyLimitRefusesWithoutAskingTheModel(t *testing.T) {
	f := &fakeLLM{answer: "ok"}
	d, _ := newDaily(2)
	c := &Ask{LLM: f, Daily: d}
	ask(t, c, 8, "one")
	ask(t, c, 8, "two") // balance 0
	only(t, "at 0", ask(t, c, 8, "three"), askDailyLimit, true)
	if len(f.got) != 2 {
		t.Errorf("the model got %d questions, want 2", len(f.got))
	}
	only(t, "another user", ask(t, c, 9, "hi"), "ok", false)
}

// A heavy answer can take the balance below 0; the next question is refused.
func TestAskDailyLimitCountsAnswerWeight(t *testing.T) {
	f := &fakeLLM{answer: "ok", usage: llm.Usage{WebSearch: true, ViewedImages: true}}
	d, _ := newDaily(3)
	c := &Ask{LLM: f, Daily: d}
	only(t, "3 left, weight 6", ask(t, c, 8, "news with pics"), "ok", false)
	if b := balance(t, d, 8); b != -3 {
		t.Errorf("balance %d, want -3", b)
	}
	only(t, "in debt", ask(t, c, 8, "again"), askDailyLimit, true)
}

func TestAskFailuresAndRefusalsCostNothing(t *testing.T) {
	d, clk := newDaily(5)
	c := &Ask{LLM: &fakeLLM{err: errors.New("down")}, Daily: d, MaxPromptChars: 10, Cooldown: 10 * time.Second, Now: clk.now}
	ask(t, c, 8, "fails")                    // provider error
	ask(t, c, 8, "  ")                       // usage hint
	ask(t, c, 8, "far too long for the cap") // too long
	c.LLM = &fakeLLM{answer: "ok"}
	clk.advance(10 * time.Second) // the failed question did reach the provider, so it started the cooldown
	only(t, "works", ask(t, c, 8, "works"), "ok", false)
	ask(t, c, 8, "cooldown") // refused by the cooldown
	if b := balance(t, d, 8); b != 4 {
		t.Errorf("balance %d, want 4: only the answered question counts", b)
	}
}

// The daily limit comes before the cooldown in what the user is told.
func TestAskDailyLimitMessageBeatsCooldown(t *testing.T) {
	d, clk := newDaily(1)
	c := &Ask{LLM: &fakeLLM{answer: "ok"}, Daily: d, Cooldown: 10 * time.Second, Now: clk.now}
	ask(t, c, 8, "one")
	only(t, "out of allowance and on cooldown", ask(t, c, 8, "two"), askDailyLimit, true)
}

// A follow-up is checked again when its turn comes: the answer it waited for
// may have used up the allowance.
func TestAskFollowUpRecheckedAgainstDailyLimit(t *testing.T) {
	llmc := newGateLLM("one")
	d, _ := newDaily(1)
	c := &Ask{LLM: llmc, Daily: d}
	first := askAsync(context.Background(), c, 8, "one")
	<-llmc.started
	second := askAsync(context.Background(), c, 8, "two")
	waitUntil(t, "the follow-up is waiting", func() bool { return c.isWaiting(8) })
	llmc.release("one")
	only(t, "first", <-first, "answer to one", false)
	only(t, "follow-up", <-second, askDailyLimit, true)
	if got := llmc.prompts(); !slices.Equal(got, []string{"one"}) {
		t.Errorf("the model got %q", got)
	}
}

func TestAskDailyStorageFailureIsSafe(t *testing.T) {
	f := &fakeLLM{answer: "ok"}
	d, _ := newDaily(5)
	d.Store.(*memBalances).fail = true
	c := &Ask{LLM: f, Daily: d}
	only(t, "storage down", ask(t, c, 8, "hi"), askFailed, true)
	if len(f.got) != 0 {
		t.Error("without a readable balance, nothing may reach the model")
	}
}

// Past the timeout the question fails with the plain error; it's neither
// charged nor remembered, and the reply still gets out.
func TestAskTimeout(t *testing.T) {
	llmc := newConvLLM("slow")
	d, _ := newDaily(5)
	mem := &ChannelMemory{MaxMessages: 10}
	c := &Ask{LLM: llmc, Daily: d, Memory: mem, Timeout: 30 * time.Millisecond}
	only(t, "timed out", askIn(t, c, 1, 8, "Alice", "slow"), askFailed, true)
	if b := balance(t, d, 8); b != 5 {
		t.Errorf("a timed-out question was charged: balance %d", b)
	}
	if got := mem.History(1); len(got) != 0 {
		t.Errorf("a timed-out question was remembered: %+v", got)
	}
}

// Waiting in line counts toward the timeout.
func TestAskTimeoutIncludesWaiting(t *testing.T) {
	llmc := newGateLLM()
	c := &Ask{LLM: llmc, MaxConcurrent: 1, Timeout: 30 * time.Millisecond}
	// Hold the only slot for the whole test (a question holding it would hit
	// the same timeout first and free it).
	c.once.Do(func() { c.slots = make(chan struct{}, 1) })
	c.slots <- struct{}{}
	only(t, "waited too long", ask(t, c, 12, "b"), askFailed, true)
	if len(llmc.prompts()) != 0 {
		t.Error("a question that timed out waiting must not reach the model")
	}
}

func TestAskAnswerCappedAtThreeMessages(t *testing.T) {
	long := strings.Repeat("word ", 1700) // 8,500 characters: 5 parts uncapped
	mem := &ChannelMemory{MaxMessages: 10, MaxTokens: 100000}
	c := &Ask{LLM: &fakeLLM{answer: long}, Memory: mem}
	got := askIn(t, c, 1, 8, "Alice", "essay please")
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	last := got[2].Content
	if !strings.HasSuffix(last, "\n…(answer cut short)") || utf8.RuneCountInString(last) > maxMessageLen {
		t.Errorf("last part: %d characters, ends %q", utf8.RuneCountInString(last), last[max(0, len(last)-30):])
	}
	h := mem.History(1)
	if len(h) != 2 || !strings.HasSuffix(h[1].Content, "(answer cut short)") {
		t.Error("memory should hold what was shown, not the full answer")
	}
}

// flakyReplier fails the sends listed in fail (counting from 1), then works.
type flakyReplier struct {
	calls int
	fail  map[int]bool
	got   []Reply
}

func (f *flakyReplier) Reply(_ context.Context, r Reply) error {
	f.calls++
	if f.fail[f.calls] {
		return errors.New("discord: 503")
	}
	f.got = append(f.got, r)
	return nil
}

// A later answer part that fails is sent once more (the first part's retry is
// the replier's): a rare double post beats an answer cut short. A second
// failure gives up, and the model is never asked again.
func TestAskResendsAFailedPartOnce(t *testing.T) {
	long := strings.Repeat("word ", 1200) // 6,000 characters: 3 parts
	llmc := &fakeLLM{answer: long}
	c := &Ask{LLM: llmc}
	rep := &flakyReplier{fail: map[int]bool{2: true}} // part 2's first try
	if err := c.Run(context.Background(), Request{ChannelID: 1, AuthorID: 8, Args: "essay", Reply: rep}); err != nil {
		t.Fatalf("one failure should be retried: %v", err)
	}
	if len(rep.got) != 3 || rep.calls != 4 {
		t.Errorf("sent %d parts in %d calls, want 3 in 4", len(rep.got), rep.calls)
	}

	rep = &flakyReplier{fail: map[int]bool{3: true, 4: true}} // part 3 fails twice
	if err := c.Run(context.Background(), Request{ChannelID: 1, AuthorID: 8, Args: "essay", Reply: rep}); err == nil || rep.calls != 4 {
		t.Errorf("err=%v calls=%d, want an error after one retry", err, rep.calls)
	}
	rep = &flakyReplier{fail: map[int]bool{1: true}} // the first part: the replier's job
	if err := c.Run(context.Background(), Request{ChannelID: 1, AuthorID: 8, Args: "essay", Reply: rep}); err == nil || rep.calls != 1 {
		t.Errorf("first part: err=%v calls=%d, want no retry here", err, rep.calls)
	}
	if n := len(llmc.got); n != 3 {
		t.Errorf("model asked %d times for 3 questions; a retry must not ask again", n)
	}
}

// A full-length third part still fits Discord's limit with the marker added.
func TestCapAnswerFullLengthLastPart(t *testing.T) {
	full := strings.Repeat("x", maxMessageLen)
	got, cut := capAnswer([]string{full, full, full, "more"})
	if !cut || len(got) != maxAnswerParts {
		t.Fatalf("cut=%v parts=%d", cut, len(got))
	}
	if n := utf8.RuneCountInString(got[2]); n != maxMessageLen || !strings.HasSuffix(got[2], askCutShort) {
		t.Errorf("last part: %d characters, marked=%v", n, strings.HasSuffix(got[2], askCutShort))
	}
}

func TestCapAnswerLeavesShortAnswersAlone(t *testing.T) {
	parts := []string{"a", "b", "c"}
	if got, cut := capAnswer(parts); cut || !slices.Equal(got, parts) {
		t.Errorf("capAnswer(3 parts) = %q, %v", got, cut)
	}
}

// -debug says why a question was refused and what it cost, never what was asked.
func TestAskDebugLogging(t *testing.T) {
	var buf bytes.Buffer
	d, _ := newDaily(1)
	c := &Ask{LLM: &fakeLLM{answer: "secret answer", usage: llm.Usage{WebSearch: true}}, Daily: d,
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	ask(t, c, 8, "secret question")
	ask(t, c, 8, "secret again")
	out := buf.String()
	for _, want := range []string{"weight=2", "balance=-1", "search=true", `reason="daily limit"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Errorf("log contains content:\n%s", out)
	}
}
