package router

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/commands"
	"github.com/kirkmadraga/vox-engine/internal/llm"
	"github.com/kirkmadraga/vox-engine/internal/remind"
	"github.com/kirkmadraga/vox-engine/internal/remind/remindtest"
)

// A repeat with an end, from setting it to its last ping: set through the
// router, listed, fired by the real scheduler and firer on a fake clock,
// marked "(last one)", then gone.
func TestE2ERemindMeUntil(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 7, 14, 0, 0, 0, zone) // a Wednesday
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	store := &remindtest.Memory{}
	e := newE2E(t, commands.RemindMe{Store: store, Location: zone, Now: now})
	e.send(e2eOwner, allowedG, "allow guild")
	e.send(e2eOwner, allowedG, "allow <@8> remindme")

	got := e.send(e2eFriend, allowedG, "remindme every 2h until 18:00 drink water")
	if len(got) != 1 || !strings.Contains(got[0].Content, "that's 2 reminders") {
		t.Fatalf("set: %+v", got)
	}
	if got := e.send(e2eFriend, allowedG, "remindme list"); len(got) != 1 || !strings.Contains(got[0].Content, ", every 2h, until <t:") {
		t.Errorf("list: %+v", got)
	}

	var sent []string
	firer := &commands.RemindFirer{
		Allowed: func(context.Context, snowflake.ID, snowflake.ID, string) bool { return true },
		Send: func(_ context.Context, _ snowflake.ID, r commands.Reply) error {
			mu.Lock()
			sent = append(sent, r.Content)
			mu.Unlock()
			return nil
		},
		Gone: func(error) bool { return false },
	}
	ticks, waits := make(chan time.Time), make(chan time.Duration, 10)
	s := &remind.Scheduler{Store: store, Fire: firer.Fire, Now: now,
		After: func(d time.Duration) <-chan time.Time { waits <- d; return ticks }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	for _, h := range []int{16, 18} {
		<-waits // asleep until the next one (at most an hour; the clock jumps anyway)
		mu.Lock()
		clock = time.Date(2026, 10, 7, h, 0, 0, 0, zone)
		mu.Unlock()
		ticks <- time.Time{}
	}
	<-waits // idle again: nothing left
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != "<@8> drink water" || sent[1] != "<@8> drink water (last one)" {
		t.Errorf("sent %q; want two, the second marked last", sent)
	}
	if rs := store.All(); len(rs) != 0 {
		t.Errorf("left %+v; want it gone after its end", rs)
	}
}

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
