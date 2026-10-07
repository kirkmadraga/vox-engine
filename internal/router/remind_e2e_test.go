package router

import (
	"strings"
	"testing"

	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/llm"
	"github.com/kirkmadraga/vox-engine/internal/remind/remindtest"
)

// remindme through the real access Policy: its own grant (play's or ask's
// don't cover it), and it isn't taken for an ask question.
func TestE2ERemindMe(t *testing.T) {
	store := &remindtest.Memory{}
	e := newE2E(t, commands.RemindMe{Store: store}, &commands.Ask{LLM: llm.Echo{}})
	e.wantReply(e2eOwner, allowedG, "allow guild", "This server is now allowed.")
	e.send(e2eOwner, allowedG, "allow <@8> play")
	e.send(e2eOwner, allowedG, "allow <@8> ask")

	e.wantSilence(e2eFriend, allowedG, "remindme in 2h stretch") // not the ask fallback either
	e.wantSilence(e2eStranger, allowedG, "remindme in 2h stretch")
	e.wantReply(e2eOwner, allowedG, "allow <@8> remindme", "<@8> can now use `remindme` in allowed servers.")
	if got := e.send(e2eFriend, allowedG, "remindme in 2h stretch"); len(got) != 1 || !strings.HasPrefix(got[0].Content, "Okay, I'll remind you") {
		t.Fatalf("granted: %+v", got)
	}
	if got := e.send(e2eFriend, allowedG, "REMINDME"); len(got) != 1 || !strings.Contains(got[0].Content, "**remindme**") {
		t.Errorf("help: %+v", got)
	}
	e.wantSilence(e2eFriend, unlistedG, "remindme in 2h stretch") // the server must be allowed too
	if rs := store.All(); len(rs) != 1 || rs[0].UserID != e2eFriend || rs[0].Text != "stretch" {
		t.Errorf("saved %+v", rs)
	}
}
