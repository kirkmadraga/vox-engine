package discordio

import (
	"testing"

	"github.com/disgoorg/disgo/gateway"
)

// A channel type Discord adds later must not fail a server's data: in disgo
// v0.19.6 it did, and the gateway reconnected and failed again until Discord
// reset the bot's token (2026-11-10, a type-20 channel).
func TestUnknownChannelTypeParses(t *testing.T) {
	guild := `{"id":"1","name":"s","channels":[{"id":"2","type":0,"name":"general"},{"id":"3","type":20,"name":"server-hub"}]}`
	if _, err := gateway.UnmarshalEventData([]byte(guild), gateway.EventTypeGuildCreate); err != nil {
		t.Errorf("GUILD_CREATE: %v", err)
	}
	channel := `{"id":"3","type":20,"guild_id":"1","name":"server-hub"}`
	if _, err := gateway.UnmarshalEventData([]byte(channel), gateway.EventTypeChannelCreate); err != nil {
		t.Errorf("CHANNEL_CREATE: %v", err)
	}
}
