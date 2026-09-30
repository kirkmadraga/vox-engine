package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// fakeLLM answers with answer (or err) and records the prompts it got.
type fakeLLM struct {
	answer string
	usage  llm.Usage
	err    error

	mu  sync.Mutex
	got []string
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Answer, error) {
	conv := req.Conversation
	f.mu.Lock()
	f.got = append(f.got, conv[len(conv)-1].Content)
	f.mu.Unlock()
	return llm.Answer{Text: f.answer, Usage: f.usage}, f.err
}

func runAsk(t *testing.T, c *Ask, args string) []Reply {
	t.Helper()
	rep := &fakeReplier{}
	if err := c.Run(context.Background(), Request{AuthorID: 8, GuildID: 100, Args: args, Reply: rep}); err != nil {
		t.Fatal(err)
	}
	return rep.got
}

func TestAskAnswers(t *testing.T) {
	f := &fakeLLM{answer: "Jazz is a music genre."}
	got := runAsk(t, &Ask{LLM: f}, "  What is jazz?  ")
	if len(f.got) != 1 || f.got[0] != "What is jazz?" {
		t.Errorf("prompt sent = %q, want [\"What is jazz?\"]", f.got)
	}
	if len(got) != 1 || got[0].Content != "Jazz is a music genre." || got[0].Mentions != nil || got[0].Private {
		t.Errorf("replies = %+v", got)
	}
}

// The answer replies to the question (first part only); slash requests have
// no message to reply to.
func TestAskAnswerRepliesToTheQuestion(t *testing.T) {
	rep := &fakeReplier{}
	c := &Ask{LLM: &fakeLLM{answer: strings.Repeat("word ", 500)}} // 2 parts
	if err := c.Run(context.Background(), Request{AuthorID: 8, MessageID: 77, Args: "hi", Reply: rep}); err != nil {
		t.Fatal(err)
	}
	if len(rep.got) != 2 || rep.got[0].ReplyTo != 77 || !rep.got[0].PingReplied || rep.got[1].ReplyTo != 0 || rep.got[1].PingReplied {
		t.Errorf("replies = %+v", rep.got)
	}
}

func TestAskEmptyPromptShowsUsage(t *testing.T) {
	f := &fakeLLM{answer: "x"}
	got := runAsk(t, &Ask{LLM: f}, "   ")
	if len(got) != 1 || got[0].Content != askUsage || !got[0].Private {
		t.Errorf("replies = %+v, want a private usage hint", got)
	}
	if len(f.got) != 0 {
		t.Error("an empty prompt must not reach the model")
	}
}

func TestAskFailureIsFriendly(t *testing.T) {
	got := runAsk(t, &Ask{LLM: &fakeLLM{err: errors.New("boom")}}, "hi")
	if len(got) != 1 || got[0].Content != askFailed || !got[0].Private {
		t.Errorf("replies = %+v, want %q", got, askFailed)
	}
}

func TestAskEmptyAnswer(t *testing.T) {
	got := runAsk(t, &Ask{LLM: &fakeLLM{answer: " \n "}}, "hi")
	if len(got) != 1 || got[0].Content != askEmpty {
		t.Errorf("replies = %+v, want %q", got, askEmpty)
	}
}

func TestAskLongAnswerIsSplit(t *testing.T) {
	answer := strings.Repeat("word ", 1000) // 5000 characters
	got := runAsk(t, &Ask{LLM: &fakeLLM{answer: answer}}, "hi")
	if len(got) != 3 {
		t.Fatalf("got %d replies, want 3", len(got))
	}
	var joined []string
	for _, r := range got {
		if n := utf8.RuneCountInString(r.Content); n > maxMessageLen {
			t.Errorf("a part has %d characters, over %d", n, maxMessageLen)
		}
		joined = append(joined, r.Content)
	}
	if strings.Join(joined, " ") != strings.TrimSpace(answer) {
		t.Error("the parts don't add up to the answer")
	}
}

func TestAskNeverLogsContent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	runAsk(t, &Ask{LLM: &fakeLLM{answer: "secret-answer"}, Logger: logger}, "secret-prompt")
	runAsk(t, &Ask{LLM: &fakeLLM{err: errors.New("boom")}, Logger: logger}, "secret-prompt")
	out := buf.String()
	if strings.Contains(out, "secret") {
		t.Errorf("log contains the prompt or answer: %s", out)
	}
	if !strings.Contains(out, "ask: answered") || !strings.Contains(out, "ask: failed") {
		t.Errorf("log is missing the outcome lines: %s", out)
	}
}

func TestSplitMessage(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want []string
	}{
		{"", 5, nil},
		{"  ", 5, nil},
		{"hello", 5, []string{"hello"}},
		{"hello world", 5, []string{"hello", "world"}},
		{"ab cd\nef gh", 8, []string{"ab cd", "ef gh"}},   // line break preferred over space
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}}, // no break: hard cut
		{"héllo wörld", 5, []string{"héllo", "wörld"}},    // counts characters, not bytes
	}
	for _, c := range cases {
		got := splitMessage(c.in, c.max)
		if strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("splitMessage(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
