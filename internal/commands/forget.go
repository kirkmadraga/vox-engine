package commands

import "context"

// ForgetCommand clears the channel's ask memory. It shares ask's access
// (grant and channels), like skip shares play's.
const ForgetCommand = "forget"

// Forget is "@Bot forget" (or /forget).
type Forget struct {
	Memory *ChannelMemory
}

func (Forget) Name() string { return ForgetCommand }

func (c Forget) Run(ctx context.Context, req Request) error {
	if c.Memory.Forget(req.ChannelID) {
		return reply(ctx, req, "Okay, I've forgotten this channel's conversation.")
	}
	return reply(ctx, req, "There was nothing to forget here.")
}
