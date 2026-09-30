// Package llm is the bot's bridge to a language model. Commands depend only on
// Provider, so the model behind it can change without touching them.
package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Role says who wrote a Message.
type Role int

const (
	User      Role = iota // someone in Discord
	Assistant             // the bot (an earlier answer)
)

// Message is one turn of a conversation. Name is the author's display name
// (never a Discord ID), so the model can tell people apart.
type Message struct {
	Role    Role
	Name    string
	Content string
	Image   *Image // an image to look at with this message, if any
}

// Image is an image the model is asked to look at.
type Image struct {
	URL         string
	ContentType string
}

// Usage is what an answer used beyond plain text generation, as the provider
// reports it. The daily ask limit weighs answers by it.
type Usage struct {
	WebSearch    bool // searched the web
	ViewedImages bool // looked at images (e.g. found while searching)
}

// Answer is a provider's reply.
type Answer struct {
	Text  string
	Usage Usage
}

// Request is one question for a provider.
type Request struct {
	// Conversation ends with the question; the earlier messages are the
	// channel's recent history, oldest first.
	Conversation []Message
	// Search allows web search for this question (if the provider has it on).
	// Without it, no search tools are offered, so the model can't search.
	Search bool
}

// Provider answers a Request.
type Provider interface {
	// Name identifies the provider in logs, e.g. "echo".
	Name() string
	Complete(ctx context.Context, req Request) (Answer, error)
}

// DefaultEchoSlow is how long an echo question marked [slow] takes.
const DefaultEchoSlow = 5 * time.Second

// Echo answers with the question itself and says how many earlier messages it
// was given. It is a stand-in for trying the command path, limits and memory
// without a real model (and without any cost). It "searches" whenever search
// is allowed. Test markers in a question: "[search]" and "[image]" report
// that usage (as a real model might), "[slow]" takes Slow to answer.
type Echo struct {
	Slow time.Duration // 0 = DefaultEchoSlow
}

func (Echo) Name() string { return "echo" }

func (e Echo) Complete(ctx context.Context, req Request) (Answer, error) {
	conversation := req.Conversation
	if err := ctx.Err(); err != nil {
		return Answer{}, err
	}
	if len(conversation) == 0 {
		return Answer{}, fmt.Errorf("echo: empty conversation")
	}
	q := conversation[len(conversation)-1].Content
	if strings.Contains(q, "[slow]") {
		slow := e.Slow
		if slow <= 0 {
			slow = DefaultEchoSlow
		}
		t := time.NewTimer(slow)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		}
	}
	text := "Echo: " + q
	last := conversation[len(conversation)-1]
	if last.Image != nil {
		text += " (with an image)"
	}
	if n := len(conversation) - 1; n == 1 {
		text += " (remembering 1 message)"
	} else if n > 1 {
		text += fmt.Sprintf(" (remembering %d messages)", n)
	}
	return Answer{Text: text, Usage: Usage{
		WebSearch:    req.Search || strings.Contains(q, "[search]"),
		ViewedImages: strings.Contains(q, "[image]") || last.Image != nil,
	}}, nil
}

// Settings configure a provider (from config.yaml and LLM_API_KEY).
type Settings struct {
	Provider           string // "echo", "xai", "openai"
	BaseURL            string
	Model              string
	APIKey             string
	Instructions       string
	MaxOutputTokens    int
	ReasoningEffort    string
	WebSearch          bool
	ImageUnderstanding bool
	MaxTurns           int
	Logger             *slog.Logger
}

// New returns the provider s names.
func New(s Settings) (Provider, error) {
	switch s.Provider {
	case "echo":
		return Echo{}, nil
	case "xai", "openai":
		return &Responses{
			ProviderName: s.Provider, BaseURL: s.BaseURL, APIKey: s.APIKey, Model: s.Model,
			Instructions: s.Instructions, MaxOutputTokens: s.MaxOutputTokens, ReasoningEffort: s.ReasoningEffort,
			WebSearch: s.WebSearch, ImageUnderstanding: s.ImageUnderstanding && s.Provider == "xai",
			MaxTurns: s.MaxTurns, Logger: s.Logger,
		}, nil
	}
	return nil, fmt.Errorf("unknown llm provider %q", s.Provider)
}
