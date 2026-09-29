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
	GuildID   snowflake.ID
	ChannelID snowflake.ID
	AuthorID  snowflake.ID
	Args      string // text after the command word, trimmed
	Reply     Replier
	// Lucky is /play's "lucky" option: play a search's first good result
	// instead of listing them. Slash-only; mentions never set it.
	Lucky bool
}

// Reply is an outgoing message. Only the users in Mentions are pinged;
// @everyone, @here and role mentions are never pinged.
type Reply struct {
	Content  string
	Mentions []snowflake.ID
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
