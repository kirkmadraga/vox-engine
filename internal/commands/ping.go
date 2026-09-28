package commands

import (
	"context"

	"github.com/disgoorg/snowflake/v2"
)

// Ping replies "@author pong".
type Ping struct{}

func (Ping) Name() string { return "ping" }

func (Ping) Run(ctx context.Context, req Request) error {
	return req.Reply.Reply(ctx, Reply{
		Content:  Mention(req.AuthorID) + " pong",
		Mentions: []snowflake.ID{req.AuthorID},
	})
}
