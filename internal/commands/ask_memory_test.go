package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// convLLM records each conversation it's given and answers "re: <question>".
// Prompts listed in gates wait until released.
type convLLM struct {
	gateLLM
	mu       sync.Mutex
	convs    [][]llm.Message
	searches []bool
	fail     bool
}

func (c *convLLM) Complete(ctx context.Context, req llm.Request) (llm.Answer, error) {
	conv := req.Conversation
	c.mu.Lock()
	c.searches = append(c.searches, req.Search)
	c.mu.Unlock()
	c.mu.Lock()
	c.convs = append(c.convs, slices.Clone(conv))
	c.mu.Unlock()
	if c.fail {
		return llm.Answer{}, errors.New("down")
	}
	if _, err := c.gateLLM.Complete(ctx, req); err != nil {
		return llm.Answer{}, err
	}
	return llm.Answer{Text: "re: " + conv[len(conv)-1].Content}, nil
}

func (c *convLLM) last() []llm.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.convs[len(c.convs)-1]
}

func newConvLLM(held ...string) *convLLM { return &convLLM{gateLLM: *newGateLLM(held...)} }

func askIn(t *testing.T, c *Ask, channel, user snowflake.ID, name, prompt string) []Reply {
	t.Helper()
	rep := &syncReplier{}
	if err := c.Run(context.Background(), Request{ChannelID: channel, AuthorID: user, AuthorName: name, Args: prompt, Reply: rep}); err != nil {
		t.Fatal(err)
	}
	return rep.replies()
}

func TestAskRemembersTheChannelsConversation(t *testing.T) {
	llmc := newConvLLM()
	c := &Ask{LLM: llmc, Memory: &ChannelMemory{MaxMessages: 10}}
	askIn(t, c, 1, 8, "Alice", "pizza in naples?")
	askIn(t, c, 1, 9, "Bob", "or salerno")
	want := []llm.Message{
		{Role: llm.User, Name: "Alice", Content: "pizza in naples?"},
		{Role: llm.Assistant, Content: "re: pizza in naples?"},
		{Role: llm.User, Name: "Bob", Content: "or salerno"},
	}
	if got := llmc.last(); !slices.Equal(got, want) {
		t.Errorf("conversation %+v, want %+v", got, want)
	}
	askIn(t, c, 2, 8, "Alice", "elsewhere")
	if got := llmc.last(); len(got) != 1 {
		t.Errorf("another channel must start fresh, got %+v", got)
	}
}

// A follow-up waits for the answer it follows, and sees it.
func TestAskFollowUpSeesTheAnswerItFollows(t *testing.T) {
	llmc := newConvLLM("one")
	c := &Ask{LLM: llmc, Memory: &ChannelMemory{MaxMessages: 10}}
	first := make(chan []Reply, 1)
	go func() { first <- askIn(t, c, 1, 8, "Alice", "one") }()
	<-llmc.started
	second := make(chan []Reply, 1)
	go func() { second <- askIn(t, c, 1, 8, "Alice", "two") }()
	waitUntil(t, "the follow-up is waiting", func() bool { return c.isWaiting(8) })
	llmc.release("one")
	<-first
	<-second
	if got := contents(llmc.last()); got != "one re: one two" {
		t.Errorf("the follow-up saw %q", got)
	}
}

// Two people asking at once: each question stays next to its own answer.
func TestAskConcurrentQuestionsKeepTheirPairs(t *testing.T) {
	llmc := newConvLLM("a", "b")
	mem := &ChannelMemory{MaxMessages: 10}
	c := &Ask{LLM: llmc, Memory: mem}
	a := make(chan []Reply, 1)
	b := make(chan []Reply, 1)
	go func() { a <- askIn(t, c, 1, 11, "A", "a") }()
	go func() { b <- askIn(t, c, 1, 12, "B", "b") }()
	<-llmc.started
	<-llmc.started
	llmc.release("b") // b answers first
	<-b
	llmc.release("a")
	<-a
	if got := contents(mem.History(1)); got != "b re: b a re: a" {
		t.Errorf("memory %q, want each question next to its answer", got)
	}
}

func TestAskRemembersOnlyAnswers(t *testing.T) {
	llmc := newConvLLM()
	mem := &ChannelMemory{MaxMessages: 10}
	c := &Ask{LLM: llmc, Memory: mem, Cooldown: 10 * time.Second, Now: newClock().now}
	askIn(t, c, 1, 8, "Alice", "  ")        // usage hint
	askIn(t, c, 1, 8, "Alice", "one")       // answered
	askIn(t, c, 1, 8, "Alice", "too early") // cooldown
	llmc.fail = true
	askIn(t, c, 1, 9, "Bob", "fails") // model down
	if got := contents(mem.History(1)); got != "one re: one" {
		t.Errorf("memory %q: hints, refusals and failures must not be remembered", got)
	}
}

func TestAskNeverLogsMemory(t *testing.T) {
	var buf bytes.Buffer
	c := &Ask{LLM: newConvLLM(), Memory: &ChannelMemory{MaxMessages: 10}, Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	askIn(t, c, 1, 8, "SecretName", "secret-one")
	askIn(t, c, 1, 8, "SecretName", "secret-two")
	if out := buf.String(); strings.Contains(out, "secret") || strings.Contains(out, "Secret") || !strings.Contains(out, "history=2") {
		t.Errorf("log: %s", out)
	}
}

func TestForget(t *testing.T) {
	mem := &ChannelMemory{MaxMessages: 10}
	c := &Ask{LLM: newConvLLM(), Memory: mem}
	askIn(t, c, 1, 8, "Alice", "one")
	f := Forget{Memory: mem}
	rep := &syncReplier{}
	f.Run(context.Background(), Request{ChannelID: 1, Reply: rep})
	f.Run(context.Background(), Request{ChannelID: 1, Reply: rep})
	got := rep.replies()
	if len(got) != 2 || got[0].Content != "Okay, I've forgotten this channel's conversation." || got[1].Content != "There was nothing to forget here." {
		t.Errorf("replies %+v", got)
	}
	if len(mem.History(1)) != 0 {
		t.Error("memory left after forget")
	}
}
