package discordio

import (
	"log/slog"
	"testing"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func TestVoiceStatesUserVoiceChannel(t *testing.T) {
	c := cache.New(cache.WithCaches(cache.FlagVoiceStates))
	inVoice := snowflake.ID(55)
	c.AddVoiceState(discord.VoiceState{GuildID: 1, UserID: 2, ChannelID: &inVoice})
	c.AddVoiceState(discord.VoiceState{GuildID: 1, UserID: 3, ChannelID: nil}) // left voice
	vs := VoiceStates{Caches: c}

	if ch, ok := vs.UserVoiceChannel(1, 2); !ok || ch != 55 {
		t.Errorf("user in voice: got %d, %v", ch, ok)
	}
	if _, ok := vs.UserVoiceChannel(1, 3); ok {
		t.Error("user with nil channel should not be in voice")
	}
	if _, ok := vs.UserVoiceChannel(1, 4); ok {
		t.Error("unknown user should not be in voice")
	}
	if _, ok := vs.UserVoiceChannel(9, 2); ok {
		t.Error("voice state from another guild must not match")
	}
}

type noCallbacks struct{}

func (noCallbacks) SendMLSKeyPackage([]byte) error        { return nil }
func (noCallbacks) SendMLSCommitWelcome([]byte) error     { return nil }
func (noCallbacks) SendReadyForTransition(uint16) error   { return nil }
func (noCallbacks) SendInvalidCommitWelcome(uint16) error { return nil }

func TestDAVEHandsEachSessionOutOnce(t *testing.T) {
	d := &DAVE{}
	if d.take() != nil {
		t.Fatal("no session before any connection")
	}
	created := d.CreateFunc()(slog.New(slog.DiscardHandler), "1", noCallbacks{})
	got := d.take()
	if got == nil || any(got) != any(created) {
		t.Fatalf("take() = %v, want the created dave-go session", got)
	}
	if d.take() != nil {
		t.Error("a session must be handed out only once")
	}
	got.Close()
}
