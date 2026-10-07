package router

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// The ask command over the real access Policy, with the echo provider.

func newAskE2E(t *testing.T) e2e {
	t.Helper()
	return newAskE2EWithMemory(t, nil)
}

// newAskE2EWithMemory is newAskE2E with channel memory (nil: none).
func newAskE2EWithMemory(t *testing.T, mem *commands.ChannelMemory) e2e {
	t.Helper()
	answers := &commands.AnswerLog{}
	e := newE2E(t, &commands.Ask{LLM: llm.Echo{}, Memory: mem, Answers: answers}, commands.Forget{Memory: mem})
	e.router.IsAnswer = answers.Has
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	return e
}

// sendIn is send from another channel.
func (e e2e) sendIn(user, channel snowflake.ID, text string) []commands.Reply {
	e.t.Helper()
	rep := &fakeReplier{}
	e.router.Handle(context.Background(), Message{GuildID: allowedG, ChannelID: channel, AuthorID: user, Content: "<@1000> " + text}, rep)
	return rep.got
}

// Memory through the whole path: echo reports how much it was given.
func TestE2EAskRemembersAndForgets(t *testing.T) {
	e := newAskE2EWithMemory(t, &commands.ChannelMemory{MaxMessages: 10})
	e.wantReply(e2eOwner, allowedG, "allow <@8> ask", "<@8> can now use `ask` in allowed servers.")
	e.wantReply(e2eFriend, allowedG, "pizza in naples?", "Echo: pizza in naples?")
	e.wantReply(e2eOwner, allowedG, "or salerno", "Echo: or salerno (remembering 2 messages)")
	if got := e.sendIn(e2eOwner, 2, "elsewhere"); len(got) != 1 || got[0].Content != "Echo: elsewhere" {
		t.Errorf("another channel must have its own memory: %+v", got)
	}

	// forget comes with ask's grant, and clears only the channel it's used in.
	e.wantSilence(e2eStranger, allowedG, "forget")
	if got := e.sendIn(e2eFriend, 2, "forget"); len(got) != 1 || got[0].Content != "Okay, I've forgotten this channel's conversation." {
		t.Errorf("forget in channel 2: %+v", got)
	}
	e.wantReply(e2eFriend, allowedG, "still?", "Echo: still? (remembering 4 messages)")
	e.wantReply(e2eFriend, allowedG, "forget", "Okay, I've forgotten this channel's conversation.")
	e.wantReply(e2eFriend, allowedG, "again", "Echo: again")
}

// ask needs only an allowed server and the grant, in any channel: there's no
// per-channel setup (removed in v1.1.0).
func TestE2EAskWorksInEveryChannel(t *testing.T) {
	e := newE2E(t, &commands.Ask{LLM: llm.Echo{}})
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.wantReply(e2eOwner, allowedG, "allow <@8> ask", "<@8> can now use `ask` in allowed servers.")
	for _, ch := range []snowflake.ID{1, 2, 3} {
		for _, text := range []string{"ask what is jazz", "what is jazz"} {
			if got := e.sendIn(e2eFriend, ch, text); len(got) != 1 || got[0].Content != "Echo: what is jazz" {
				t.Errorf("channel %d, %q: %+v", ch, text, got)
			}
			if got := e.sendIn(e2eStranger, ch, text); len(got) != 0 {
				t.Errorf("no grant, channel %d, %q: want silence, got %+v", ch, text, got)
			}
		}
	}
	// The old channel setup is just an unknown form of allow now.
	e.wantReply(e2eOwner, allowedG, "allow ask", "Usage: `allow guild [serverID]` (defaults to this server), `allow @user [command]`, "+
		"`allow everyone [command]` (everyone in this server) or `allow public [command]` (everyone, in every server); the command defaults to `play`.")
}

func TestE2EAskFallback(t *testing.T) {
	e := newAskE2E(t)
	// A first word that isn't a command: the whole text, case kept, is the prompt.
	e.wantReply(e2eOwner, allowedG, "What is  Jazz?", "Echo: What is  Jazz?")
	e.wantReply(e2eOwner, allowedG, "dance", "Echo: dance")
	// Explicit "ask": the words after it are the prompt.
	e.wantReply(e2eOwner, allowedG, "ASK what is jazz", "Echo: what is jazz")
	// Known commands still win.
	e.wantReply(e2eOwner, allowedG, "ping hello", "<@7> pong")
}

func TestE2EAskNeedsItsOwnGrant(t *testing.T) {
	e := newAskE2E(t)
	e.send(e2eOwner, allowedG, "allow <@8> play")

	// Not public, and play's grant doesn't cover it: silent, both ways in.
	e.wantSilence(e2eFriend, allowedG, "what is jazz")
	e.wantSilence(e2eFriend, allowedG, "ask what is jazz")
	e.wantSilence(e2eStranger, allowedG, "what is jazz")

	e.wantReply(e2eOwner, allowedG, "allow <@8> ask", "<@8> can now use `ask` in allowed servers.")
	e.wantReply(e2eFriend, allowedG, "what is jazz", "Echo: what is jazz")
	e.wantReply(e2eFriend, allowedG, "ask what is jazz", "Echo: what is jazz")
	e.wantSilence(e2eFriend, unlistedG, "what is jazz") // the server must be allowed too

	e.wantReply(e2eOwner, allowedG, "deny <@8> ask", "<@8> can no longer use `ask`.")
	e.wantSilence(e2eFriend, allowedG, "what is jazz")
}

func TestE2EAskOffKeepsUnknownCommand(t *testing.T) {
	// Fallback set but no ask command registered (no LLM configured): as before.
	e := newE2E(t)
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.wantReply(e2eOwner, allowedG, "what is jazz", "unknown command")
	e.wantSilence(e2eStranger, allowedG, "what is jazz")
}

// The asking message's ID travels from the mention to the answer, which replies to it.
func TestE2EAskAnswerRepliesToTheMention(t *testing.T) {
	e := newAskE2E(t)
	rep := &fakeReplier{}
	e.router.Handle(context.Background(), Message{GuildID: allowedG, ChannelID: 1, AuthorID: e2eOwner, MessageID: 99, Content: "<@1000> what is jazz"}, rep)
	if len(rep.got) != 1 || rep.got[0].ReplyTo != 99 || rep.got[0].Content != "Echo: what is jazz" {
		t.Errorf("replies = %+v", rep.got)
	}
}

// ask sends "<@bot> text" in channel 1 and returns the answer's message ID.
func (e e2e) askFor(user snowflake.ID, text, want string) snowflake.ID {
	e.t.Helper()
	rep := &fakeReplier{}
	e.router.Handle(context.Background(), Message{GuildID: allowedG, ChannelID: 1, AuthorID: user, Content: "<@1000> " + text}, rep)
	if len(rep.got) != 1 || rep.got[0].Content != want {
		e.t.Fatalf("%q: replies %+v, want %q", text, rep.got, want)
	}
	return rep.ids[0]
}

// replyTo sends a Discord reply (no leading mention, @ ping on) to message to.
func (e e2e) replyTo(user, channel, to snowflake.ID, text string) *fakeReplier {
	e.t.Helper()
	return e.reply(user, channel, to, text, true)
}

func (e e2e) reply(user, channel, to snowflake.ID, text string, ping bool) *fakeReplier {
	e.t.Helper()
	msg := Message{GuildID: allowedG, ChannelID: channel, AuthorID: user, ReplyToID: to, Content: text}
	if ping {
		msg.Mentions = []snowflake.ID{self}
	}
	rep := &fakeReplier{}
	e.router.Handle(context.Background(), msg, rep)
	return rep
}

// Replying to an ask answer (with the reply's @ ping on, so the text arrives)
// continues the conversation, under the same rules as a mention.
func TestE2EReplyToAnAnswerContinues(t *testing.T) {
	e := newAskE2EWithMemory(t, &commands.ChannelMemory{MaxMessages: 10})
	e.wantReply(e2eOwner, allowedG, "allow <@8> ask", "<@8> can now use `ask` in allowed servers.")
	answer := e.askFor(e2eFriend, "pizza in naples?", "Echo: pizza in naples?")

	rep := e.replyTo(e2eFriend, 1, answer, "or salerno")
	if len(rep.got) != 1 || rep.got[0].Content != "Echo: or salerno (remembering 2 messages)" {
		t.Fatalf("reply to an answer: %+v", rep.got)
	}
	// The new answer can be replied to in turn.
	if again := e.replyTo(e2eFriend, 1, rep.ids[0], "and sorrento"); len(again.got) != 1 ||
		again.got[0].Content != "Echo: and sorrento (remembering 4 messages)" {
		t.Errorf("reply to the reply's answer: %+v", again.got)
	}

	// ask works in channel 2 too, so only the answer check keeps it quiet there.
	cases := []struct {
		name    string
		user    snowflake.ID
		channel snowflake.ID
		to      snowflake.ID
		text    string
	}{
		{"reply to a message that isn't an answer", e2eFriend, 1, 999, "hi"},
		{"blank reply (@ ping off: Discord sends no text)", e2eFriend, 1, answer, ""},
		{"no ask grant", e2eStranger, 1, answer, "hi"},
		{"the answer's ID in another channel", e2eFriend, 2, answer, "hi"},
	}
	for _, c := range cases {
		if rep := e.replyTo(c.user, c.channel, c.to, c.text); len(rep.got) != 0 {
			t.Errorf("%s: want silence, got %+v", c.name, rep.got)
		}
	}

	// With the Message Content intent a reply with the @ ping off arrives with
	// its text; it still mustn't trigger the bot.
	if rep := e.reply(e2eFriend, 1, answer, "just chatting", false); len(rep.got) != 0 {
		t.Errorf("ping-off reply with text: %+v", rep.got)
	}

	// A reply that starts with a mention is an ordinary command.
	if rep := e.replyTo(e2eFriend, 1, answer, "<@1000> ping"); len(rep.got) != 1 || rep.got[0].Content != "<@8> pong" {
		t.Errorf("mention in a reply: %+v", rep.got)
	}
}

// dailyStore is an in-memory commands.DailyBalances.
type dailyStore map[snowflake.ID]int

func (d dailyStore) DailyBalance(_ context.Context, user snowflake.ID) (string, int, bool, error) {
	b, ok := d[user]
	return time.Now().UTC().Format(time.DateOnly), b, ok, nil
}

func (d dailyStore) AllDailyBalances(context.Context) ([]commands.DailyRow, error) {
	var out []commands.DailyRow
	for u, b := range d {
		out = append(out, commands.DailyRow{User: u, Day: time.Now().UTC().Format(time.DateOnly), Balance: b})
	}
	return out, nil
}

func (d dailyStore) SetDailyBalance(_ context.Context, user snowflake.ID, _ string, b int) error {
	d[user] = b
	return nil
}

// The daily limit through the whole path, weighted by echo's test markers.
func TestE2EAskDailyLimit(t *testing.T) {
	store := dailyStore{}
	daily := &commands.DailyLimit{Limit: 3, WeightSearch: 2, WeightImage: 4, Store: store,
		IsOwner: func(id snowflake.ID) bool { return id == e2eOwner }}
	e := newE2E(t, &commands.Ask{LLM: llm.Echo{}, Daily: daily, MaxPromptChars: 20})
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.wantReply(e2eOwner, allowedG, "allow <@8> ask", "<@8> can now use `ask` in allowed servers.")

	e.wantReply(e2eFriend, allowedG, "news [search]", "Echo: news [search]") // 3 - 2 = 1
	e.wantReply(e2eFriend, allowedG, "pics [image]", "Echo: pics [image]")   // 1 - 4 = -3
	if store[e2eFriend] != -3 {
		t.Errorf("balance %d, want -3", store[e2eFriend])
	}
	e.wantReply(e2eFriend, allowedG, "more", "You've hit your daily ask limit for today.")
	e.wantReply(e2eFriend, allowedG, "this question is far too long", "That's too long, keep it under 20 characters.")
	e.wantReply(e2eOwner, allowedG, "owners [image]", "Echo: owners [image]") // exempt by default
	if _, charged := store[e2eOwner]; charged {
		t.Error("an exempt owner was charged")
	}
}

// The replied-to message travels from the mention to the model.
func TestE2EAskQuotesARepliedToMessage(t *testing.T) {
	e := newAskE2E(t)
	rep := &fakeReplier{}
	e.router.Handle(context.Background(), Message{GuildID: allowedG, ChannelID: 1, AuthorID: e2eOwner,
		Content: "<@1000> is this true?", Mentions: []snowflake.ID{self}, ReplyToID: 50,
		Quoted: &commands.Quote{MessageID: 50, AuthorName: "user1", Content: "the moon is made of cheese"}}, rep)
	want := "Echo: [Replying to user1: \"the moon is made of cheese\"]\nis this true?"
	if len(rep.got) != 1 || rep.got[0].Content != want {
		t.Errorf("replies %+v, want %q", rep.got, want)
	}
}

// After a restart the bot has no list of its answers, but an ask answer is
// still recognisable: the bot's own message that is itself a reply. The
// forgotten answer is quoted so the reply keeps its context.
func TestE2EReplyToAnAnswerFromBeforeARestart(t *testing.T) {
	e := newAskE2EWithMemory(t, &commands.ChannelMemory{MaxMessages: 10}) // fresh: knows no answers
	e.wantReply(e2eOwner, allowedG, "allow <@8> ask", "<@8> can now use `ask` in allowed servers.")
	reply := func(q *commands.Quote, text string) []commands.Reply {
		rep := &fakeReplier{}
		e.router.Handle(context.Background(), Message{GuildID: allowedG, ChannelID: 1, AuthorID: e2eFriend,
			Mentions: []snowflake.ID{self}, ReplyToID: q.MessageID, Quoted: q, Content: text}, rep)
		return rep.got
	}
	old := &commands.Quote{MessageID: 900, AuthorID: self, AuthorName: "Vox Engine", AuthorBot: true, IsReply: true, Content: "Try the margherita."}
	if got := reply(old, "and the drinks?"); len(got) != 1 ||
		got[0].Content != "Echo: [Replying to Vox Engine: \"Try the margherita.\"]\nand the drinks?" {
		t.Errorf("reply to an answer from before a restart: %+v", got)
	}
	// The bot's other messages aren't replies ("Now playing…", lists, errors): ignored.
	nowPlaying := &commands.Quote{MessageID: 901, AuthorID: self, AuthorName: "Vox Engine", AuthorBot: true, Content: "Now playing: x"}
	if got := reply(nowPlaying, "nice song"); len(got) != 0 {
		t.Errorf("reply to a non-answer bot message: %+v", got)
	}
	// Someone else's reply isn't an ask answer, even if it pings the bot.
	other := &commands.Quote{MessageID: 902, AuthorID: 77, AuthorName: "Bob", IsReply: true, Content: "hi"}
	if got := reply(other, "hello"); len(got) != 0 {
		t.Errorf("reply to someone else's reply: %+v", got)
	}
}
