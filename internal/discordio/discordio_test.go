package discordio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"bot/internal/commands"
)

type fakeSender struct {
	channel snowflake.ID
	msg     discord.MessageCreate
	calls   int
}

func (f *fakeSender) CreateMessage(ch snowflake.ID, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.channel, f.msg = ch, m
	f.calls++
	return &discord.Message{}, nil
}

func TestChannelReplierRestrictsMentions(t *testing.T) {
	s := &fakeSender{}
	r := ChannelReplier{Sender: s, ChannelID: 55}
	err := r.Reply(context.Background(), commands.Reply{Content: "<@9> pong @everyone", Mentions: []snowflake.ID{9}})
	if err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || s.channel != 55 {
		t.Fatalf("calls=%d channel=%d", s.calls, s.channel)
	}
	body, err := json.Marshal(s.msg.AllowedMentions)
	if err != nil {
		t.Fatal(err)
	}
	// Discord expects arrays here; null would be invalid and nil Parse would mean "parse everything".
	want := `{"parse":[],"roles":[],"users":["9"],"replied_user":false}`
	if string(body) != want {
		t.Errorf("allowed_mentions = %s, want %s", body, want)
	}
}

func TestChannelReplierNoMentions(t *testing.T) {
	s := &fakeSender{}
	if err := (ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{Content: "unknown command"}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(s.msg.AllowedMentions)
	if !strings.Contains(string(body), `"users":[]`) {
		t.Errorf("allowed_mentions = %s, want empty users array", body)
	}
}
