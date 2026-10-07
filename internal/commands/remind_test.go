package commands

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
	"github.com/kirkmadraga/vox-engine/internal/remind"
	"github.com/kirkmadraga/vox-engine/internal/remind/remindtest"
)

// Wednesday 2026-10-07 14:00 in UTC+8.
var remindNow = time.Date(2026, 10, 7, 14, 0, 0, 0, zone8)

func newRemindMe() (RemindMe, *remindtest.Memory, *int) {
	store := &remindtest.Memory{}
	woken := new(int)
	return RemindMe{Store: store, Location: zone8, Now: func() time.Time { return remindNow }, Wake: func() { *woken++ }}, store, woken
}

// remindIn runs remindme as user 8 in guild 100, channel 5, and returns the one reply.
func remindIn(t *testing.T, c RemindMe, guild snowflake.ID, req Request) Reply {
	t.Helper()
	rep := &fakeReplier{}
	req.GuildID, req.ChannelID, req.AuthorID, req.AuthorName, req.Reply = guild, 5, 8, "Alice", rep
	if err := c.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(rep.got) != 1 || !rep.got[0].Private || len(rep.got[0].Mentions) != 0 {
		t.Fatalf("%+v: replies %+v, want one private reply that pings no one", req, rep.got)
	}
	return rep.got[0]
}

func remindArgs(t *testing.T, c RemindMe, args string) string {
	t.Helper()
	return remindIn(t, c, 100, Request{Args: args}).Content
}

func TestRemindMeSets(t *testing.T) {
	c, store, woken := newRemindMe()
	got := remindArgs(t, c, "in 2h stretch your legs")
	due := remindNow.Add(2 * time.Hour)
	if want := fmt.Sprintf("Okay, I'll remind you <t:%d:F> (<t:%d:R>). It's number `1`.", due.Unix(), due.Unix()); got != want {
		t.Errorf("reply %q, want %q", got, want)
	}
	rs := store.All()
	if len(rs) != 1 || rs[0].UserID != 8 || rs[0].UserName != "Alice" || rs[0].GuildID != 100 || rs[0].ChannelID != 5 ||
		rs[0].Text != "stretch your legs" || !rs[0].Next.Equal(due) || !rs[0].Rule.Once() || rs[0].Location != "UTC+8" || *woken != 1 {
		t.Fatalf("saved %+v, woken %d", rs, *woken)
	}

	got = remindArgs(t, c, "every day at 08:00 standup")
	if !strings.Contains(got, ", then every day at 08:00 (UTC+8). It's number `2`.") {
		t.Errorf("repeating: %q", got)
	}
	// Slash: the when apart from the text, so the text can start with anything.
	got = remindIn(t, c, 100, Request{When: "at 18:30", Args: "at last, dinner"}).Content
	if rs := store.All(); !strings.HasPrefix(got, "Okay") || rs[1].Text != "at last, dinner" || !rs[1].Next.Equal(remindNow.Add(270*time.Minute)) {
		t.Errorf("slash: %q, saved %+v", got, rs)
	}
}

// Every mistake gets the help, with what was wrong when that's known.
func TestRemindMeHelpsWithMistakes(t *testing.T) {
	c, store, _ := newRemindMe()
	cases := map[string]string{
		"":                  "**remindme**",
		"help":              "**remindme**",
		"soon stretch":      "I couldn't tell when you mean.",
		"at 6 stretch":      "I couldn't set that: write the time as 18:30 or 6:30pm.",
		"in 400d stretch":   "I couldn't set that: that's more than a year away.",
		"every 30m stretch": "I couldn't set that: repeats must be at least 1h apart.",
		"in 2h":             "What should I remind you about?",
		"cancel":            "Which one?",
		"cancel two":        "Which one?",
	}
	for args, want := range cases {
		got := remindArgs(t, c, args)
		if !strings.Contains(got, want) || !strings.Contains(got, "Write `remindme <when> <what>`") {
			t.Errorf("%q: %q, want %q and the help", args, got, want)
		}
	}
	if got := remindIn(t, c, 100, Request{When: "in 2h stretch", Args: "x"}).Content; !strings.Contains(got, "I couldn't tell when") {
		t.Errorf("slash when with extra words: %q", got)
	}
	if got := remindArgs(t, c, "in 2h "+strings.Repeat("x", 301)); !strings.Contains(got, "under 300 characters") {
		t.Errorf("too long: %q", got)
	}
	if len(store.All()) != 0 {
		t.Errorf("mistakes saved reminders: %+v", store.All())
	}
	if !strings.Contains(c.help(), "Times are UTC+8.") {
		t.Errorf("the help must say the timezone: %q", c.help())
	}
}

func TestRemindMeLimit(t *testing.T) {
	c, store, _ := newRemindMe()
	c.MaxPerUser = 2
	remindArgs(t, c, "in 1h a")
	remindIn(t, c, 200, Request{Args: "in 1h b"}) // the limit counts every server
	if got := remindArgs(t, c, "in 1h c"); !strings.Contains(got, "You already have 2 reminders") || len(store.All()) != 2 {
		t.Errorf("over the limit: %q", got)
	}
}

func TestRemindMeListAndCancel(t *testing.T) {
	c, store, _ := newRemindMe()
	if got := remindArgs(t, c, "list"); !strings.Contains(got, "no reminders in this server") {
		t.Errorf("empty list: %q", got)
	}
	remindArgs(t, c, "in 2h stretch <@9> @everyone")
	remindArgs(t, c, "every weekday at 9am standup")
	remindIn(t, c, 200, Request{Args: "in 1h elsewhere"})
	store.AddReminder(context.Background(), remind.Reminder{UserID: 9, GuildID: 100, ChannelID: 5, Text: "not yours", Next: remindNow.Add(time.Hour)})

	got := remindArgs(t, c, "LIST")
	for _, want := range []string{"**Your reminders here** (2), plus 1 in other servers", "`1` <t:", "in <#5>: stretch <@9> @everyone",
		", every weekday at 09:00: standup", "remindme cancel <number>"} {
		if !strings.Contains(got, want) {
			t.Errorf("list lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "elsewhere") || strings.Contains(got, "not yours") {
		t.Errorf("list shows others' or other servers' reminders:\n%s", got)
	}

	if got := remindArgs(t, c, "cancel 4"); !strings.Contains(got, "You don't have a reminder `4`") {
		t.Errorf("cancelling someone else's: %q", got)
	}
	if got := remindArgs(t, c, "cancel #1"); got != "Cancelled reminder `1`." {
		t.Errorf("cancel: %q", got)
	}
	if got := remindArgs(t, c, "cancel 3"); got != "Cancelled reminder `3`." { // own, from another server
		t.Errorf("cancel from another server: %q", got)
	}
	if n := len(store.All()); n != 2 {
		t.Errorf("left %d, want 2", n)
	}
}

func TestRemindMeStoreFailures(t *testing.T) {
	c, store, _ := newRemindMe()
	store.Fail = errors.New("disk full")
	for _, args := range []string{"in 1h x", "cancel 1"} {
		if got := remindArgs(t, c, args); got != saveFailed {
			t.Errorf("%q: %q", args, got)
		}
	}
	if got := remindArgs(t, c, "list"); !strings.Contains(got, "Couldn't read your reminders") {
		t.Errorf("list: %q", got)
	}
}

// firerEnv: a RemindFirer whose access, model and sends are recorded.
type firerEnv struct {
	f       *RemindFirer
	allowed map[string]bool // command -> allowed for user 8
	sent    []Reply
	sendErr []error // returned by successive sends
	llm     *fakeLLM
	daily   *DailyLimit
}

var errChannelGone = errors.New("unknown channel")

func newFirer(withAsk bool) *firerEnv {
	e := &firerEnv{allowed: map[string]bool{RemindCommand: true, AskCommand: true}, llm: &fakeLLM{answer: "  Hey Alice,\ntime to stretch!  "}}
	e.f = &RemindFirer{
		Allowed: func(_ context.Context, user, guild snowflake.ID, command string) bool {
			return user == 8 && guild == 100 && e.allowed[command]
		},
		Send: func(_ context.Context, ch snowflake.ID, r Reply) error {
			e.sent = append(e.sent, r)
			if len(e.sendErr) > 0 {
				err := e.sendErr[0]
				e.sendErr = e.sendErr[1:]
				return err
			}
			return nil
		},
		Gone: func(err error) bool { return errors.Is(err, errChannelGone) },
	}
	if withAsk {
		e.daily, _ = newDaily(2)
		e.f.Ask = &Ask{LLM: e.llm, Daily: e.daily}
	}
	return e
}

var reminder = remind.Reminder{ID: 1, UserID: 8, UserName: "Alice", GuildID: 100, ChannelID: 5, Text: "stretch <@9> @everyone"}

func (e *firerEnv) fire(t *testing.T, late bool) error {
	t.Helper()
	return e.f.Fire(context.Background(), reminder, late)
}

func TestFirerSendsTheOwnTextWithoutAsk(t *testing.T) {
	e := newFirer(false)
	if err := e.fire(t, false); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 || e.sent[0].Content != "<@8> stretch <@9> @everyone" || !slices.Equal(e.sent[0].Mentions, []snowflake.ID{8}) {
		t.Errorf("sent %+v; want the own text, pinging only its owner", e.sent)
	}
	if st := e.f.Stats(); st.Plain != 1 || st.Worded != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestFirerWordsItWithAskAndCharges(t *testing.T) {
	e := newFirer(true)
	e.fire(t, false)
	if len(e.sent) != 1 || e.sent[0].Content != "<@8> Hey Alice, time to stretch!" {
		t.Fatalf("sent %+v", e.sent)
	}
	if len(e.llm.got) != 1 || !strings.Contains(e.llm.got[0], `"stretch <@9> @everyone"`) || !strings.HasPrefix(e.llm.got[0], "[reminder]") {
		t.Errorf("prompt %q", e.llm.got)
	}
	if b := balance(t, e.daily, 8); b != 1 {
		t.Errorf("balance %d, want 1 (charged like a plain answer)", b)
	}
	if st := e.f.Stats(); st.Worded != 1 {
		t.Errorf("stats %+v", st)
	}
}

// No ask grant, no allowance or a failing model: the own text, free.
func TestFirerFallsBackToTheOwnText(t *testing.T) {
	cases := map[string]func(e *firerEnv){
		"no ask grant": func(e *firerEnv) { e.allowed[AskCommand] = false },
		"no allowance": func(e *firerEnv) {
			e.daily.Charge(context.Background(), 8, e.llm.usage)
			e.daily.Charge(context.Background(), 8, e.llm.usage)
		},
		"model fails": func(e *firerEnv) { e.llm.err = errors.New("402: out of credits") },
		"empty words": func(e *firerEnv) { e.llm.answer = "  " },
	}
	for name, setup := range cases {
		e := newFirer(true)
		setup(e)
		before := balance(t, e.daily, 8)
		if err := e.fire(t, false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(e.sent) != 1 || e.sent[0].Content != "<@8> stretch <@9> @everyone" {
			t.Errorf("%s: sent %+v", name, e.sent)
		}
		if b := balance(t, e.daily, 8); b != before {
			t.Errorf("%s: charged (%d -> %d)", name, before, b)
		}
		if name == "no ask grant" && len(e.llm.got) != 0 {
			t.Errorf("no ask grant: the model was asked")
		}
	}
}

func TestFirerMarksLateOnes(t *testing.T) {
	e := newFirer(false)
	e.fire(t, true)
	if !strings.HasSuffix(e.sent[0].Content, " (late: I was offline when it was due)") {
		t.Errorf("late: %q", e.sent[0].Content)
	}
}

func TestFirerGoneAndRetries(t *testing.T) {
	e := newFirer(false)
	e.allowed[RemindCommand] = false
	if err := e.fire(t, false); !errors.Is(err, remind.ErrGone) || len(e.sent) != 0 {
		t.Errorf("no remindme access: %v, sent %+v", err, e.sent)
	}

	e = newFirer(false)
	e.sendErr = []error{errors.New("503")}
	if err := e.fire(t, false); err != nil || len(e.sent) != 2 {
		t.Errorf("one failure: %v after %d sends, want a retry that works", err, len(e.sent))
	}
	e = newFirer(false)
	e.sendErr = []error{errors.New("503"), errors.New("503")}
	if err := e.fire(t, false); err == nil || errors.Is(err, remind.ErrGone) || len(e.sent) != 2 {
		t.Errorf("two failures: %v after %d sends", err, len(e.sent))
	}
	e = newFirer(false)
	e.sendErr = []error{errChannelGone}
	if err := e.fire(t, false); !errors.Is(err, remind.ErrGone) || len(e.sent) != 1 {
		t.Errorf("channel gone: %v after %d sends, want gone without a retry", err, len(e.sent))
	}
}

// Whenever remindme is taken away (from a person, a server, everyone in a
// server, or the public), exactly the reminders that lost their access are
// deleted: the ones still reachable some other way, and owners', stay.
func TestDenyDeletesRemindersThatLostAccess(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	store := &remindtest.Memory{}
	deny := Deny{Access: e.policy, Reminders: store}
	allow := Allow{Access: e.policy, Known: func(string) bool { return true }}
	const other snowflake.ID = 200 // a server that isn't allowed
	type key struct{ user, guild snowflake.ID }
	add := func(user, guild snowflake.ID) {
		store.AddReminder(context.Background(), remind.Reminder{UserID: user, GuildID: guild, Text: "x", Next: remindNow})
	}
	left := func(stage string, want ...key) {
		t.Helper()
		var got []key
		for _, r := range store.All() {
			got = append(got, key{r.UserID, r.GuildID})
		}
		slices.SortFunc(got, func(a, b key) int { return cmp.Or(cmp.Compare(a.user, b.user), cmp.Compare(a.guild, b.guild)) })
		slices.SortFunc(want, func(a, b key) int { return cmp.Or(cmp.Compare(a.user, b.user), cmp.Compare(a.guild, b.guild)) })
		if !slices.Equal(got, want) {
			t.Errorf("%s: left %v, want %v", stage, got, want)
		}
	}
	reply := func(cmd Command, args, wantSuffix string) {
		t.Helper()
		if got := run(t, cmd, args); !strings.HasSuffix(got.Content, wantSuffix) {
			t.Errorf("%s %s: %q, want it to end with %q", cmd.Name(), args, got.Content, wantSuffix)
		}
	}
	noNote := func(cmd Command, args string) {
		t.Helper()
		if got := run(t, cmd, args); strings.Contains(got.Content, "reminder") {
			t.Errorf("%s %s: %q, want no reminder note", cmd.Name(), args, got.Content)
		}
	}
	ctx := context.Background()
	e.policy.AllowGuild(ctx, guildID, ownerID)
	e.policy.Grant(ctx, 8, RemindCommand, ownerID)
	e.policy.Grant(ctx, 8, "play", ownerID)
	add(8, guildID)
	add(8, other)
	add(9, guildID) // no access at all: set while remindme was open, say
	add(ownerID, guildID)
	add(ownerID, other)

	// Another command: nothing is checked or deleted, even 9's stale one.
	noNote(deny, "<@8> play")
	left("deny play", key{8, guildID}, key{8, other}, key{9, guildID}, key{ownerID, guildID}, key{ownerID, other})

	// remindme open to everyone here: 8 keeps this server's, loses the other's.
	allow.Run(ctx, Request{GuildID: guildID, AuthorID: ownerID, Args: "everyone remindme", Reply: &fakeReplier{}})
	reply(deny, "<@8> remindme", "can no longer use `remindme`.\n1 reminder that went with it was deleted.")
	left("revoke 8", key{8, guildID}, key{9, guildID}, key{ownerID, guildID}, key{ownerID, other})

	// Public: denying the server deletes nothing, since public still reaches it.
	run(t, allow, "public remindme")
	reply(deny, "guild", "Only owners and public commands work there now.\nIts open command (`remindme`) comes back if you allow it again.")
	left("deny guild while public", key{8, guildID}, key{9, guildID}, key{ownerID, guildID}, key{ownerID, other})

	// Closing public with the server denied: everyone's here goes, owners' stay.
	reply(deny, "public remindme", "is no longer open to everyone, in every server I'm in.\n2 reminders that went with it were deleted.")
	left("deny public", key{ownerID, guildID}, key{ownerID, other})

	// Closing everyone in an allowed server: only those without a grant go.
	run(t, allow, "guild")
	e.policy.Grant(ctx, 8, RemindCommand, ownerID)
	add(8, guildID)
	add(9, guildID)
	reply(deny, "everyone remindme", "is no longer open to everyone in this server.\n1 reminder that went with it was deleted.")
	left("deny everyone", key{8, guildID}, key{ownerID, guildID}, key{ownerID, other})

	// Denying the server now takes 8's too.
	reply(deny, "guild", "\n1 reminder that went with it was deleted.")
	left("deny guild", key{ownerID, guildID}, key{ownerID, other})

	// Closing something else, or nothing at all, checks nothing.
	run(t, allow, "everyone play")
	add(9, guildID)
	noNote(deny, "everyone play")
	reply(deny, "everyone remindme", "wasn't open to everyone in this server.")
	left("no-ops", key{9, guildID}, key{ownerID, guildID}, key{ownerID, other})
}

// A storage error while checking must never read as "no access": nothing is
// deleted, and the reply says the check didn't finish.
func TestDenyKeepsRemindersWhenTheCheckFails(t *testing.T) {
	backend := &flakyOpen{Memory: accesstest.NewMemory()}
	e := newEnv(backend)
	store := &remindtest.Memory{}
	deny := Deny{Access: e.policy, Reminders: store}
	ctx := context.Background()
	e.policy.Grant(ctx, 8, RemindCommand, ownerID)
	store.AddReminder(ctx, remind.Reminder{UserID: 8, GuildID: guildID, Text: "x", Next: remindNow})
	store.AddReminder(ctx, remind.Reminder{UserID: 9, GuildID: guildID, Text: "x", Next: remindNow})

	backend.fail = true
	if got := run(t, deny, "<@8> remindme"); !strings.HasSuffix(got.Content, "Couldn't check all the reminders that went with it; any left will be dropped when due.") {
		t.Errorf("access check failing: %q", got.Content)
	}
	if n := len(store.All()); n != 2 {
		t.Errorf("deleted %d reminders on a failed check", 2-n)
	}

	backend.fail = false
	e.policy.Grant(ctx, 8, RemindCommand, ownerID)
	store.Fail = errors.New("disk gone")
	if got := run(t, deny, "<@8> remindme"); !strings.Contains(got.Content, "Couldn't check all the reminders") {
		t.Errorf("reminder store failing: %q", got.Content)
	}
}

// flakyOpen fails the open-commands lookup on demand.
type flakyOpen struct {
	*accesstest.Memory
	fail bool
}

func (f *flakyOpen) CommandOpen(ctx context.Context, guild snowflake.ID, command string) (bool, error) {
	if f.fail {
		return false, errors.New("storage down")
	}
	return f.Memory.CommandOpen(ctx, guild, command)
}
