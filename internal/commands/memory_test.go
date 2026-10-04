package commands

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

func qa(i int) (llm.Message, llm.Message) {
	return llm.Message{Role: llm.User, Name: "Kirk", Content: fmt.Sprintf("q%d", i)},
		llm.Message{Role: llm.Assistant, Content: fmt.Sprintf("a%d", i)}
}

func contents(ms []llm.Message) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.Content)
	}
	return strings.Join(s, " ")
}

func addN(m *ChannelMemory, ch snowflake.ID, from, to int) {
	for i := from; i <= to; i++ {
		q, a := qa(i)
		m.Add(ch, snowflake.ID(i), q, a)
	}
}

// Over the message limit, the oldest half goes at once, so the conversation's
// start stays put for the next few questions (prompt caching keeps working).
func TestMemoryDropsTheOldestHalfAtOnce(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 10}
	addN(m, 1, 1, 5)
	if got := contents(m.History(1)); got != "q1 a1 q2 a2 q3 a3 q4 a4 q5 a5" {
		t.Fatalf("5 exchanges: %s", got)
	}
	addN(m, 1, 6, 6)
	if got := contents(m.History(1)); got != "q4 a4 q5 a5 q6 a6" {
		t.Fatalf("after the 6th: %s", got)
	}
	addN(m, 1, 7, 8)
	if got := contents(m.History(1)); got != "q4 a4 q5 a5 q6 a6 q7 a7 q8 a8" {
		t.Errorf("the start must stay the same while under the limit: %s", got)
	}
}

// An odd MaxMessages rounds down to whole question-and-answer pairs: 11 keeps
// 5, like 10.
func TestMemoryOddMaxMessages(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 11}
	addN(m, 1, 1, 5)
	if got := contents(m.History(1)); got != "q1 a1 q2 a2 q3 a3 q4 a4 q5 a5" {
		t.Fatalf("5 exchanges: %s", got)
	}
	addN(m, 1, 6, 6)
	if got := contents(m.History(1)); got != "q4 a4 q5 a5 q6 a6" {
		t.Errorf("after the 6th (as with 10): %s", got)
	}
}

func TestMemoryForgetsOldMessages(t *testing.T) {
	clk := newClock()
	m := &ChannelMemory{MaxMessages: 10, MaxAge: 10 * time.Minute, Now: clk.now}
	addN(m, 1, 1, 1)
	clk.advance(6 * time.Minute)
	addN(m, 1, 2, 2)
	clk.advance(5 * time.Minute) // q1 is 11m old, q2 5m
	if got := contents(m.History(1)); got != "q2 a2" {
		t.Errorf("11 minutes later: %s", got)
	}
	clk.advance(6 * time.Minute)
	if got := m.History(1); len(got) != 0 {
		t.Errorf("a quiet channel must age out entirely: %s", contents(got))
	}
}

func TestMemoryTokenCap(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 100, MaxTokens: 100}
	long := strings.Repeat("x", 200) // ~50 tokens
	for i := range 6 {
		m.Add(1, 0, llm.Message{Content: fmt.Sprint(i)}, llm.Message{Content: long})
	}
	if xs := m.channels[1]; tokens(xs) > 100 || len(xs) == 0 {
		t.Errorf("%d exchanges, ~%d tokens; want some, within 100", len(xs), tokens(xs))
	}
	m.Add(1, 0, llm.Message{Content: "huge"}, llm.Message{Content: strings.Repeat("x", 2000)})
	if got := m.History(1); len(got) != 0 {
		t.Errorf("an exchange over the cap on its own isn't kept: %d messages", len(got))
	}
}

func TestMemoryOff(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 0}
	addN(m, 1, 1, 3)
	if got := m.History(1); len(got) != 0 {
		t.Errorf("remembered %s with memory off", contents(got))
	}
	var nilMem *ChannelMemory // no memory configured: all calls are safe no-ops
	nilMem.Add(1, 1, llm.Message{}, llm.Message{})
	if nilMem.History(1) != nil || nilMem.Forget(1) {
		t.Error("a nil memory must be empty")
	}
	nilMem.ForgetMessage(1, 1)
}

func TestMemoryPerChannel(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 10}
	addN(m, 1, 1, 2)
	addN(m, 2, 3, 3)
	if contents(m.History(1)) != "q1 a1 q2 a2" || contents(m.History(2)) != "q3 a3" {
		t.Errorf("channels mixed: %s | %s", contents(m.History(1)), contents(m.History(2)))
	}
	if !m.Forget(1) || m.Forget(1) {
		t.Error("Forget should report whether there was anything")
	}
	if len(m.History(1)) != 0 || contents(m.History(2)) != "q3 a3" {
		t.Error("forgetting one channel must not touch another")
	}
}

// A question deleted in Discord is dropped with its answer.
func TestMemoryForgetsDeletedQuestions(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 10}
	addN(m, 1, 1, 3) // question i was message i
	m.ForgetMessage(1, 2)
	m.ForgetMessage(1, 0)  // slash questions have no message: ignored
	m.ForgetMessage(2, 3)  // another channel: nothing to drop
	m.ForgetMessage(1, 99) // not remembered: nothing to drop
	if got := contents(m.History(1)); got != "q1 a1 q3 a3" {
		t.Errorf("after deleting q2: %s", got)
	}
}

func TestMemoryKeepsRolesAndNames(t *testing.T) {
	m := &ChannelMemory{MaxMessages: 10}
	m.Add(1, 5, llm.Message{Role: llm.User, Name: "Alice", Content: "hi"}, llm.Message{Role: llm.Assistant, Content: "hello"})
	got := m.History(1)
	want := []llm.Message{{Role: llm.User, Name: "Alice", Content: "hi"}, {Role: llm.Assistant, Content: "hello"}}
	if !slices.Equal(got, want) {
		t.Errorf("history %+v, want %+v", got, want)
	}
}

// Answers' message IDs are remembered with their exchange, and forgotten with it.
func TestMemoryKnowsAnswerMessages(t *testing.T) {
	clk := newClock()
	m := &ChannelMemory{MaxMessages: 10, MaxAge: 5 * time.Minute, Now: clk.now}
	q, a := qa(1)
	seq := m.Add(1, 10, q, a)
	m.AddAnswerMessage(1, seq, 11)
	m.AddAnswerMessage(1, seq, 12) // a second part
	m.AddAnswerMessage(1, 999, 13) // unknown exchange: ignored
	for id, want := range map[snowflake.ID]bool{10: true, 11: true, 12: true, 13: false} {
		if got := m.Has(1, id); got != want {
			t.Errorf("Has(%d) = %v, want %v", id, got, want)
		}
	}
	if m.Has(2, 11) {
		t.Error("another channel")
	}
	clk.advance(6 * time.Minute)
	if m.Has(1, 11) {
		t.Error("an aged-out answer must not count as remembered (so it gets quoted)")
	}
}
