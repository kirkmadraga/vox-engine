package llm

import (
	"context"
	"testing"
	"time"
)

func q(s string) Message { return Message{Role: User, Name: "Kirk", Content: s} }

func TestEcho(t *testing.T) {
	cases := []struct {
		conv []Message
		want string
	}{
		{[]Message{q("What is jazz?")}, "Echo: What is jazz?"},
		{[]Message{q("a"), {Role: Assistant, Content: "b"}, q("c")}, "Echo: c (remembering 2 messages)"},
		{[]Message{{Role: Assistant, Content: "b"}, q("c")}, "Echo: c (remembering 1 message)"},
	}
	for _, c := range cases {
		got, err := (Echo{}).Complete(context.Background(), Request{Conversation: c.conv})
		if err != nil || got.Text != c.want || got.Usage != (Usage{}) {
			t.Errorf("Complete = (%+v, %v), want %q and no usage", got, err, c.want)
		}
	}
	if _, err := (Echo{}).Complete(context.Background(), Request{}); err == nil {
		t.Error("want an error for an empty conversation")
	}
}

func TestEchoUsageMarkers(t *testing.T) {
	cases := map[string]Usage{
		"news [search]":          {WebSearch: true},
		"cat pics [image]":       {ViewedImages: true},
		"[search] and [image] x": {WebSearch: true, ViewedImages: true},
		"search image":           {},
	}
	for prompt, want := range cases {
		if got, _ := (Echo{}).Complete(context.Background(), Request{Conversation: []Message{q(prompt)}}); got.Usage != want {
			t.Errorf("%q: usage %+v, want %+v", prompt, got.Usage, want)
		}
	}
}

func TestEchoSlow(t *testing.T) {
	start := time.Now()
	if _, err := (Echo{Slow: 30 * time.Millisecond}).Complete(context.Background(), Request{Conversation: []Message{q("hi [slow]")}}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Error("[slow] answered too fast")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := (Echo{Slow: time.Hour}).Complete(ctx, Request{Conversation: []Message{q("hi [slow]")}}); err == nil {
		t.Error("a slow echo must give up when the context ends")
	}
}

func TestEchoCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Echo{}).Complete(ctx, Request{Conversation: []Message{q("hi")}}); err == nil {
		t.Fatal("want an error for a cancelled context")
	}
}

func TestNew(t *testing.T) {
	p, err := New(Settings{Provider: "echo"})
	if err != nil || p.Name() != "echo" {
		t.Fatalf("New(echo) = (%v, %v)", p, err)
	}
	if _, err := New(Settings{Provider: "gpt"}); err == nil {
		t.Fatal("want an error for an unknown provider")
	}
	for _, name := range []string{"xai", "openai"} {
		p, err := New(Settings{Provider: name, ImageUnderstanding: true})
		r, ok := p.(*Responses)
		if err != nil || !ok || r.Name() != name {
			t.Fatalf("New(%s) = (%v, %v)", name, p, err)
		}
		if r.ImageUnderstanding != (name == "xai") {
			t.Errorf("%s: image understanding %v (xAI-only option)", name, r.ImageUnderstanding)
		}
	}
}
