package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/disgoorg/snowflake/v2"
)

// Command is one bot command, dispatched by its lowercase Name.
type Command interface {
	Name() string
	Run(ctx context.Context, req Request) error
}

// Request is what a command receives: who asked, where, and how to answer.
type Request struct {
	GuildID    snowflake.ID
	ChannelID  snowflake.ID
	AuthorID   snowflake.ID
	AuthorName string       // display name (server nickname if set)
	MessageID  snowflake.ID // the message that asked; 0 for slash commands
	Args       string       // text after the command word, trimmed
	Reply      Replier
	// Lucky is /play's "lucky" option: play a search's first good result
	// instead of listing them. Slash-only; mentions never set it.
	Lucky bool
	// Search is /ask's "search" option: allow a web search for this question.
	// Slash-only; mentions start the question with "search" instead.
	Search bool
	// When is /remindme set's "when" option, kept apart from the text (Args)
	// so nothing has to be guessed. Slash-only; mentions write it first.
	When string
	// Images are the asker's attached images, in order (mentions only).
	Images []Image
	// Quoted is the message this one replies to, when Discord included it
	// (it needs the Message Content intent for other people's messages).
	Quoted *Quote
}

// Image is an attached image. The URL is Discord's, never logged.
type Image struct {
	URL         string
	ContentType string
	Size        int
}

// Quote is a message someone replied to when asking.
type Quote struct {
	MessageID  snowflake.ID
	AuthorID   snowflake.ID
	AuthorName string
	AuthorBot  bool
	Self       bool // written by this bot (set by the router)
	IsReply    bool // the quoted message is itself a Discord reply
	Content    string
	Images     []Image
}

// Reply is an outgoing message. Only the users in Mentions are pinged;
// @everyone, @here and role mentions are never pinged.
type Reply struct {
	Content  string
	Mentions []snowflake.ID
	// ReplyTo, if set, sends this as a Discord reply to that message. Its
	// author is pinged only with PingReplied. Slash replies ignore both.
	ReplyTo     snowflake.ID
	PingReplied bool
	// Private asks for a reply only the user sees. Slash replies honour it;
	// mention replies can't be private and send a normal message.
	Private bool
	// Sent, if set, is called with the ID of the (non-private) message sent.
	Sent func(messageID snowflake.ID)
}

// TypingIndicator is implemented by repliers that can show "Bot is typing…"
// while a command works. stop ends this command's share of it.
type TypingIndicator interface {
	StartTyping() (stop func())
}

// privateReply sends content as a private reply where possible.
func privateReply(ctx context.Context, req Request, content string) error {
	return req.Reply.Reply(ctx, Reply{Content: content, Private: true})
}

// Replier sends a message to the channel the request came from.
type Replier interface {
	Reply(ctx context.Context, r Reply) error
}

// Mention formats a user mention for message content.
func Mention(id snowflake.ID) string {
	return "<@" + id.String() + ">"
}

// Registry maps command names to commands.
type Registry struct {
	byName map[string]Command
}

// NewRegistry builds a registry, rejecting empty or duplicate names.
func NewRegistry(cmds ...Command) (*Registry, error) {
	r := &Registry{byName: make(map[string]Command, len(cmds))}
	for _, c := range cmds {
		name := strings.ToLower(c.Name())
		if name == "" {
			return nil, fmt.Errorf("command %T has an empty name", c)
		}
		if _, dup := r.byName[name]; dup {
			return nil, fmt.Errorf("duplicate command name %q", name)
		}
		r.byName[name] = c
	}
	return r, nil
}

// Lookup finds a command by name, case-insensitively.
func (r *Registry) Lookup(name string) (Command, bool) {
	c, ok := r.byName[strings.ToLower(name)]
	return c, ok
}
