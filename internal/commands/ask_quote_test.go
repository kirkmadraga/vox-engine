package commands

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// Replying to someone else's message while asking: the operator's rules.
//   - The replied-to message is quoted to the model, unless it's already in
//     the conversation (an earlier question, or one of the bot's answers).
//   - Its text counts toward the question length limit.
//   - At most one image: the asker's first, else the replied-to message's first.
//   - An image question is refused up front if the allowance can't cover it.

func askWith(t *testing.T, c *Ask, req Request) []Reply {
	t.Helper()
	rep := &syncReplier{}
	req.Reply = rep
	if req.AuthorID == 0 {
		req.AuthorID = 8
	}
	if err := c.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return rep.replies()
}

func img(url string) Image { return Image{URL: url, ContentType: "image/png"} }

func TestAskQuotesTheRepliedToMessage(t *testing.T) {
	llmc := newConvLLM()
	c := &Ask{LLM: llmc}
	askWith(t, c, Request{ChannelID: 1, Args: "is this true?",
		Quoted: &Quote{MessageID: 50, AuthorName: "user1", Content: "the moon is made of cheese"}})
	got := llmc.last()[0].Content
	if got != "[Replying to user1: \"the moon is made of cheese\"]\nis this true?" {
		t.Errorf("question sent: %q", got)
	}
}

func TestAskDoesntQuoteWhatsAlreadyInTheConversation(t *testing.T) {
	llmc := newConvLLM()
	mem := &ChannelMemory{MaxMessages: 10}
	answers := &AnswerLog{}
	c := &Ask{LLM: llmc, Memory: mem, Answers: answers}
	rep := &idReplier{next: 60} // the answer goes out as message 61
	if err := c.Run(context.Background(), Request{ChannelID: 1, AuthorID: 8, MessageID: 60, Args: "first question", Reply: rep}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []snowflake.ID{60, 61} { // the question, and the bot's answer
		askWith(t, c, Request{ChannelID: 1, AuthorID: 9, Args: "and?", Quoted: &Quote{MessageID: id, AuthorName: "x", Content: strings.Repeat("long ", 200)}})
		if q := llmc.last(); q[len(q)-1].Content != "and?" {
			t.Errorf("message %d is in memory already, but was quoted: %q", id, q[len(q)-1].Content)
		}
	}
}

// One of the bot's answers that memory no longer has (older than the window,
// or from before a restart) is quoted: its end, not counted toward the limit.
func TestAskQuotesItsOwnForgottenAnswer(t *testing.T) {
	llmc := newConvLLM()
	c := &Ask{LLM: llmc, Memory: &ChannelMemory{MaxMessages: 10}, MaxPromptChars: 500}
	answer := strings.Repeat("a", 1500) + "THE END"
	got := askWith(t, c, Request{ChannelID: 1, Args: strings.Repeat("q", 450),
		Quoted: &Quote{MessageID: 70, AuthorName: "Vox Engine", Self: true, IsReply: true, Content: answer}})
	if len(got) != 1 || got[0].Private {
		t.Fatalf("a long own answer plus a 450-character reply must not be refused: %+v", got)
	}
	sent := llmc.last()[0].Content
	if !strings.HasPrefix(sent, "[Replying to Vox Engine: \"…") || !strings.Contains(sent, "THE END") ||
		utf8.RuneCountInString(sent) > maxSelfQuote+450+50 {
		t.Errorf("quote sent (%d chars): %.80q…", utf8.RuneCountInString(sent), sent)
	}
}

// 800 + 500 characters: over the limit together, with a message that says so.
func TestAskQuoteCountsTowardLength(t *testing.T) {
	llmc := newConvLLM()
	c := &Ask{LLM: llmc, MaxPromptChars: 500}
	got := askWith(t, c, Request{ChannelID: 1, Args: strings.Repeat("b", 20),
		Quoted: &Quote{MessageID: 50, AuthorName: "user1", Content: strings.Repeat("a", 800)}})
	only(t, "long quote", got, askQuoteTooLongMessage(500), true)
	only(t, "fits together", askWith(t, c, Request{ChannelID: 1, AuthorID: 9, Args: strings.Repeat("b", 100),
		Quoted: &Quote{MessageID: 51, AuthorName: "user1", Content: strings.Repeat("a", 400)}}), "re: [Replying to user1: \""+strings.Repeat("a", 400)+"\"]\n"+strings.Repeat("b", 100), false)
	if len(llmc.prompts()) != 1 {
		t.Errorf("the model got %d questions, want 1", len(llmc.prompts()))
	}
}

func TestAskPicksOneImage(t *testing.T) {
	cases := []struct {
		name  string
		own   []Image
		quote *Quote
		want  string // URL, "" for none
	}{
		{"the asker's first of two", []Image{img("a1"), img("a2")}, &Quote{MessageID: 5, Images: []Image{img("q1")}}, "a1"},
		{"the replied-to message's when the asker has none", nil, &Quote{MessageID: 5, Images: []Image{img("q1"), img("q2")}}, "q1"},
		{"the asker's, with no quote", []Image{img("a1")}, nil, "a1"},
		{"none", nil, &Quote{MessageID: 5, Content: "text only"}, ""},
	}
	for _, c := range cases {
		llmc := newConvLLM()
		a := &Ask{LLM: llmc}
		askWith(t, a, Request{ChannelID: 1, Args: "what's this?", Images: c.own, Quoted: c.quote})
		got := llmc.last()[len(llmc.last())-1].Image
		switch {
		case c.want == "" && got != nil, c.want != "" && (got == nil || got.URL != c.want):
			t.Errorf("%s: image %+v, want %q", c.name, got, c.want)
		}
	}
}

// An image question costs the image weight, known before asking.
func TestAskImageQuestionsAndTheDailyLimit(t *testing.T) {
	llmc := newConvLLM()
	d, _ := newDaily(5)
	c := &Ask{LLM: llmc, Daily: d}
	only(t, "5 left, image costs 4", askWith(t, c, Request{ChannelID: 1, Args: "meme?", Images: []Image{img("m")}}), "re: meme?", false)
	if b := balance(t, d, 8); b != 1 {
		t.Errorf("balance %d, want 1", b)
	}
	only(t, "1 left", askWith(t, c, Request{ChannelID: 1, Args: "another?", Images: []Image{img("m")}}), askDailyLimitImage, true)
	only(t, "text still fine", askWith(t, c, Request{ChannelID: 1, Args: "text?"}), "re: text?", false)
	if n := len(llmc.prompts()); n != 2 {
		t.Errorf("the model got %d questions, want 2", n)
	}
}

// Memory keeps "[image]" as text: follow-ups don't resend (or pay for) it.
func TestAskRemembersImagesAsText(t *testing.T) {
	llmc := newConvLLM()
	mem := &ChannelMemory{MaxMessages: 10}
	c := &Ask{LLM: llmc, Memory: mem}
	askWith(t, c, Request{ChannelID: 1, Args: "meme?", Images: []Image{img("m")}})
	h := mem.History(1)
	if len(h) != 2 || h[0].Content != "meme? [image]" || h[0].Image != nil {
		t.Errorf("memory: %+v", h)
	}
}

// Echo marks an image question as having viewed images.
func TestEchoSeesTheImage(t *testing.T) {
	got, _ := llm.Echo{}.Complete(context.Background(), llm.Request{Conversation: []llm.Message{{Content: "hi", Image: &llm.Image{URL: "x"}}}})
	if got.Text != "Echo: hi (with an image)" || !got.Usage.ViewedImages {
		t.Errorf("echo: %+v", got)
	}
}
