package main

import (
	"slices"
	"testing"

	"github.com/disgoorg/disgo/gateway"

	"github.com/kirkmadraga/vox-engine/internal/config"
	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// Reminders get their own model settings only when configured, never search
// or look at images, and otherwise keep ask's settings.
func TestReminderSettings(t *testing.T) {
	ask := llm.Settings{Provider: "xai", Model: "grok-4.7", ReasoningEffort: "low", WebSearch: true, ImageUnderstanding: true,
		MaxTurns: 2, MaxOutputTokens: 500, Instructions: "be a tsundere", APIKey: "k", BaseURL: "https://api.x.ai/v1"}
	if _, ok := reminderSettings(config.Config{LLMProvider: "xai"}, ask); ok {
		t.Error("nothing set: reminders must use ask's provider")
	}
	if _, ok := reminderSettings(config.Config{LLMProvider: "echo", LLMReminderModel: "m"}, ask); ok {
		t.Error("echo has no models to choose")
	}
	for _, c := range []struct {
		cfg              config.Config
		model, reasoning string
	}{
		{config.Config{LLMProvider: "xai", LLMReminderModel: "grok-4.3", LLMReminderReasoningEffort: "none"}, "grok-4.3", "none"},
		{config.Config{LLMProvider: "xai", LLMReminderModel: "grok-4.3"}, "grok-4.3", ""},           // the model's default, not ask's "low"
		{config.Config{LLMProvider: "xai", LLMReminderReasoningEffort: "none"}, "grok-4.7", "none"}, // ask's model, its own effort
	} {
		got, ok := reminderSettings(c.cfg, ask)
		want := ask
		want.Model, want.ReasoningEffort, want.WebSearch, want.ImageUnderstanding = c.model, c.reasoning, false, false
		if !ok || got != want {
			t.Errorf("%+v:\n got  %+v\n want %+v", c.cfg, got, want)
		}
	}
}

// The privileged Message Content intent is requested only when configured:
// asking for it while it's off in the Developer Portal stops the bot connecting.
func TestIntents(t *testing.T) {
	if slices.Contains(intents(config.Config{}), gateway.IntentMessageContent) {
		t.Error("Message Content must not be requested by default")
	}
	if !slices.Contains(intents(config.Config{DiscordMessageContent: true}), gateway.IntentMessageContent) {
		t.Error("discord_message_content: true must request it")
	}
	for _, need := range []gateway.Intents{gateway.IntentGuilds, gateway.IntentGuildMessages, gateway.IntentGuildVoiceStates} {
		if !slices.Contains(intents(config.Config{}), need) {
			t.Errorf("missing intent %v", need)
		}
	}
}
