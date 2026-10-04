package discordio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
)

type fakeSender struct {
	channel  snowflake.ID
	msg      discord.MessageCreate
	calls    int
	sent     []discord.MessageCreate
	failRefs bool  // refuse replies, as Discord does when the replied-to message is gone
	refErr   error // the error for a refused reply (default: Discord's 400)
}

func (f *fakeSender) CreateMessage(ch snowflake.ID, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.channel, f.msg = ch, m
	f.calls++
	f.sent = append(f.sent, m)
	if f.failRefs && m.MessageReference != nil {
		if f.refErr != nil {
			return nil, f.refErr
		}
		return nil, &rest.Error{Response: &http.Response{StatusCode: http.StatusBadRequest}, Code: 50035, Message: "Invalid Form Body"}
	}
	return &discord.Message{ID: snowflake.ID(100 + f.calls)}, nil
}

// The ID of the message actually sent is reported (after a retry, the retry's).
func TestChannelReplierReportsSentID(t *testing.T) {
	for _, failRefs := range []bool{false, true} {
		s := &fakeSender{failRefs: failRefs}
		var got []snowflake.ID
		(ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{
			Content: "a", ReplyTo: 42, Sent: func(id snowflake.ID) { got = append(got, id) },
		})
		want := snowflake.ID(100 + s.calls)
		if len(got) != 1 || got[0] != want {
			t.Errorf("failRefs=%v: sent IDs %v, want [%d]", failRefs, got, want)
		}
	}
}

func TestRepliedTo(t *testing.T) {
	id := snowflake.ID(42)
	ref := &discord.MessageReference{MessageID: &id}
	cases := []struct {
		m    discord.Message
		want snowflake.ID
	}{
		{discord.Message{Type: discord.MessageTypeReply, MessageReference: ref}, 42},
		{discord.Message{Type: discord.MessageTypeDefault, MessageReference: ref}, 0}, // e.g. a forward
		{discord.Message{Type: discord.MessageTypeReply}, 0},
	}
	for i, c := range cases {
		if got := repliedTo(c.m); got != c.want {
			t.Errorf("case %d: %d, want %d", i, got, c.want)
		}
	}
}

func TestChannelReplierRepliesWithoutPinging(t *testing.T) {
	s := &fakeSender{}
	if err := (ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{Content: "answer", ReplyTo: 42}); err != nil {
		t.Fatal(err)
	}
	ref := s.msg.MessageReference
	if s.calls != 1 || ref == nil || ref.MessageID == nil || *ref.MessageID != 42 {
		t.Fatalf("calls=%d reference=%+v", s.calls, ref)
	}
	if s.msg.AllowedMentions == nil || s.msg.AllowedMentions.RepliedUser {
		t.Errorf("a reply must not ping the asker: %+v", s.msg.AllowedMentions)
	}
}

func TestChannelReplierCanPingTheRepliedUser(t *testing.T) {
	s := &fakeSender{}
	(ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{Content: "answer", ReplyTo: 42, PingReplied: true})
	body, _ := json.Marshal(s.msg.AllowedMentions)
	if want := `{"parse":[],"roles":[],"users":[],"replied_user":true}`; string(body) != want {
		t.Errorf("allowed_mentions = %s, want %s (the asker only; still no @everyone or roles)", body, want)
	}
}

func TestChannelReplierFallsBackWhenQuestionIsGone(t *testing.T) {
	s := &fakeSender{failRefs: true}
	if err := (ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{Content: "answer", ReplyTo: 42}); err != nil {
		t.Fatalf("want the plain retry to succeed, got %v", err)
	}
	if s.calls != 2 || s.msg.MessageReference != nil || s.msg.Content != "answer" {
		t.Errorf("calls=%d last=%+v", s.calls, s.msg)
	}
}

// Any failed reply is re-sent as a plain message, not only Discord's refusal:
// the operator prefers a rare double post to an answer that never shows.
func TestChannelReplierRetriesAnyFailure(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		errors.New("connection reset"),
		&rest.Error{Response: &http.Response{StatusCode: http.StatusBadGateway}},
	} {
		s := &fakeSender{failRefs: true, refErr: err}
		got := (ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{Content: "answer", ReplyTo: 42})
		if got != nil || s.calls != 2 || s.msg.MessageReference != nil {
			t.Errorf("%v: err=%v calls=%d, want one plain re-send", err, got, s.calls)
		}
	}
}

func TestChannelReplierPlainByDefault(t *testing.T) {
	s := &fakeSender{}
	(ChannelReplier{Sender: s, ChannelID: 1}).Reply(context.Background(), commands.Reply{Content: "pong"})
	if s.calls != 1 || s.msg.MessageReference != nil {
		t.Errorf("calls=%d reference=%+v", s.calls, s.msg.MessageReference)
	}
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

func TestChannelNotifierPingsNoOne(t *testing.T) {
	s := &fakeSender{}
	ChannelNotifier{Sender: s}.Notify(77, "Now playing: **x**, requested by <@5>. @everyone")
	if s.calls != 1 || s.channel != 77 {
		t.Fatalf("calls=%d channel=%d", s.calls, s.channel)
	}
	body, _ := json.Marshal(s.msg.AllowedMentions)
	if string(body) != `{"parse":[],"roles":[],"users":[],"replied_user":false}` {
		t.Errorf("allowed_mentions = %s", body)
	}
}

func TestBotVoiceLeaveHandlerOnlyForSelf(t *testing.T) {
	var got []snowflake.ID
	h := BotVoiceLeaveHandler(func() snowflake.ID { return 1000 }, func(g snowflake.ID) { got = append(got, g) })
	leave := func(user, guild snowflake.ID) *events.GuildVoiceLeave {
		return &events.GuildVoiceLeave{GenericGuildVoiceState: &events.GenericGuildVoiceState{
			VoiceState: discord.VoiceState{UserID: user, GuildID: guild},
		}}
	}
	h(leave(5, 1))    // someone else left
	h(leave(1000, 2)) // the bot left
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("onLeave calls = %v, want [2]", got)
	}
}

func TestImagesKeepsOnlyImagesInOrder(t *testing.T) {
	ct := func(s string) *string { return &s }
	got := images([]discord.Attachment{
		{URL: "a", ContentType: ct("application/pdf")},
		{URL: "b", ContentType: ct("image/png"), Size: 10},
		{URL: "c"}, // no type: skipped
		{URL: "d", ContentType: ct("image/jpeg")},
	})
	if len(got) != 2 || got[0].URL != "b" || got[0].Size != 10 || got[1].URL != "d" {
		t.Errorf("images = %+v", got)
	}
}

func TestQuoted(t *testing.T) {
	id := snowflake.ID(42)
	ct := "image/png"
	ref := &discord.Message{ID: 42, Content: "the moon is made of cheese", Author: discord.User{Username: "user1"},
		Attachments: []discord.Attachment{{URL: "x", ContentType: &ct}}}
	reply := discord.Message{Type: discord.MessageTypeReply, MessageReference: &discord.MessageReference{MessageID: &id}, ReferencedMessage: ref}
	q := quoted(reply)
	if q == nil || q.MessageID != 42 || q.AuthorName != "user1" || q.Content != "the moon is made of cheese" || len(q.Images) != 1 {
		t.Fatalf("quoted = %+v", q)
	}
	// Without the Message Content intent Discord blanks it: nothing to quote.
	reply.ReferencedMessage = &discord.Message{ID: 42}
	if quoted(reply) != nil {
		t.Error("a blank referenced message must not be quoted")
	}
	if quoted(discord.Message{Content: "not a reply"}) != nil {
		t.Error("not a reply: nothing to quote")
	}
}

func TestMentionIDs(t *testing.T) {
	got := mentionIDs(discord.Message{Mentions: []discord.User{{ID: 7}, {ID: 1000}}})
	if len(got) != 2 || got[0] != 7 || got[1] != 1000 {
		t.Errorf("mentionIDs = %v", got)
	}
}
