package main

import (
	"slices"
	"testing"

	"github.com/disgoorg/disgo/gateway"

	"github.com/kirkmadraga/vox-engine/internal/config"
)

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
