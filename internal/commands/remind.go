package commands

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/remind"
)

// RemindCommand is "remindme": set, list and cancel reminders.
const RemindCommand = "remindme"

// Defaults for RemindMe's limits (the bot operator's choices).
const (
	DefaultMaxReminders  = 10  // active per person, across servers
	DefaultReminderChars = 300 // longest reminder text
)

// RemindMe is "@Bot remindme <when> <text>", "remindme list" and "remindme
// cancel <number>" (and /remindme set|list|cancel). A reminder pings its
// owner in the channel where it was set; see RemindFirer for how it's sent.
// Replies are private on slash commands. The text is never logged.
type RemindMe struct {
	Store        remind.Store
	Location     *time.Location   // where "at 18:30" is; nil = UTC
	Wake         func()           // tells the scheduler a reminder was added; nil = none
	MaxPerUser   int              // 0 = DefaultMaxReminders
	MaxTextChars int              // 0 = DefaultReminderChars
	Now          func() time.Time // nil = time.Now
	Logger       *slog.Logger
}

func (RemindMe) Name() string { return RemindCommand }

// remindHelp is shown for "remindme" alone and after any mistake.
func (c RemindMe) help() string {
	return "**remindme**: I'll ping you here when it's time. Write `remindme <when> <what>`:\n" +
		"`remindme in 45m stretch`: in 45 minutes (also 1h30m, 3d, 2w)\n" +
		"`remindme at 18:30 call mom`: today, or tomorrow if it's passed (also 6:30pm)\n" +
		"`remindme tomorrow at 9am pay rent`: tomorrow (09:00 without a time)\n" +
		"`remindme on friday at 20:00 game night`: the coming Friday (today, if it's Friday before 20:00)\n" +
		"`remindme on 2026-12-24 wrap gifts`: that date (12-24: the next one)\n" +
		"`remindme every 2h drink water`: over and over, at least an hour apart\n" +
		"`remindme every day at 08:00 standup`: also every weekday, weekend, monday or mon,thu\n" +
		"`remindme every 2h until 18:00 drink water`: a repeat that stops (also until friday, until 12-24, for 3d)\n" +
		"`remindme list`: your reminders here\n" +
		"`remindme cancel 12`: remove one, by its number from the list\n" +
		fmt.Sprintf("Times are %s. Up to %d reminders, %d characters each.", c.location(), c.maxPerUser(), c.maxChars())
}

func (c RemindMe) Run(ctx context.Context, req Request) error {
	args := strings.TrimSpace(req.Args)
	if req.When == "" {
		word, rest, _ := strings.Cut(args, " ")
		switch strings.ToLower(word) {
		case "", "help":
			return privateReply(ctx, req, c.help())
		case "list":
			return c.list(ctx, req)
		case "cancel":
			return c.cancel(ctx, req, strings.TrimSpace(rest))
		}
	}
	return c.set(ctx, req, args)
}

func (c RemindMe) set(ctx context.Context, req Request, args string) error {
	var when remind.When
	var text string
	var err error
	if req.When != "" { // slash: the when on its own, the text in Args
		var rest string
		when, rest, err = remind.Parse(req.When, c.now(), c.location())
		if err == nil && rest != "" {
			err = remind.ErrUsage
		}
		text = args
	} else {
		when, text, err = remind.Parse(args, c.now(), c.location())
	}
	text = strings.TrimSpace(text)
	switch {
	case err != nil:
		return privateReply(ctx, req, c.mistake(err))
	case text == "":
		return privateReply(ctx, req, "What should I remind you about? Write it after the time.\n\n"+c.help())
	case utf8.RuneCountInString(text) > c.maxChars():
		return privateReply(ctx, req, fmt.Sprintf("That's too long, keep it under %d characters.", c.maxChars()))
	}
	mine, err := c.Store.UserReminders(ctx, req.AuthorID)
	if err != nil {
		c.logger().Error("remindme: couldn't read reminders", "user", req.AuthorID, "err", err)
		return privateReply(ctx, req, saveFailed)
	}
	if len(mine) >= c.maxPerUser() {
		return privateReply(ctx, req, fmt.Sprintf("You already have %d reminders, the most I keep. Cancel one first (`remindme list`).", len(mine)))
	}
	id, err := c.Store.AddReminder(ctx, remind.Reminder{
		UserID: req.AuthorID, UserName: req.AuthorName, GuildID: req.GuildID, ChannelID: req.ChannelID,
		Text: text, Rule: when.Rule, Location: c.location().String(), Next: when.At, Until: when.Until, Created: c.now(),
	})
	if err != nil {
		c.logger().Error("remindme: couldn't save", "user", req.AuthorID, "err", err)
		return privateReply(ctx, req, saveFailed)
	}
	c.logger().Info("remindme: set", "reminder", id, "user", req.AuthorID, "guild", req.GuildID, "channel", req.ChannelID,
		"due", when.At.UTC(), "repeats", !when.Rule.Once(), "ends", !when.Until.IsZero(), "text_len", utf8.RuneCountInString(text))
	if c.Wake != nil {
		c.Wake()
	}
	msg := fmt.Sprintf("Okay, I'll remind you <t:%d:F> (<t:%d:R>)", when.At.Unix(), when.At.Unix())
	if !when.Rule.Once() {
		msg += ", then " + when.Rule.String()
		if when.Rule.Every == 0 {
			msg += " (" + c.location().String() + ")"
		}
	}
	if !when.Until.IsZero() {
		msg += fmt.Sprintf(", until <t:%d:f>%s", when.Until.Unix(), c.count(when))
	}
	return privateReply(ctx, req, fmt.Sprintf("%s. It's number `%d`.", msg, id))
}

// maxListedTimes is how many reminder times a confirmation spells out.
const maxListedTimes = 5

// count says how many times a repeat with an end will fire, listing them
// when there are a few: "that's 2 reminders: 16:00 and 18:00". Times on the
// first one's day show as a time of day only.
func (c RemindMe) count(w remind.When) string {
	times, ok := w.Times(c.location(), maxListedTimes)
	if !ok {
		return fmt.Sprintf(": more than %d reminders", maxListedTimes)
	}
	if len(times) == 1 {
		return ": that's the only one"
	}
	first := times[0].In(c.location())
	parts := make([]string, len(times))
	for i, t := range times {
		style := "f"
		if l := t.In(c.location()); l.Year() == first.Year() && l.YearDay() == first.YearDay() {
			style = "t"
		}
		parts[i] = fmt.Sprintf("<t:%d:%s>", t.Unix(), style)
	}
	return fmt.Sprintf(": that's %d reminders, %s and %s", len(times), strings.Join(parts[:len(parts)-1], ", "), parts[len(parts)-1])
}

// mistake explains a when that wasn't understood, with the help.
func (c RemindMe) mistake(err error) string {
	if err == remind.ErrUsage {
		return "I couldn't tell when you mean.\n\n" + c.help()
	}
	return "I couldn't set that: " + err.Error() + ".\n\n" + c.help()
}

// list shows the user's reminders set in this server (others' channels stay
// out of a reply that may be public).
func (c RemindMe) list(ctx context.Context, req Request) error {
	mine, err := c.Store.UserReminders(ctx, req.AuthorID)
	if err != nil {
		c.logger().Error("remindme: couldn't read reminders", "user", req.AuthorID, "err", err)
		return privateReply(ctx, req, "Couldn't read your reminders. Details are in the bot's log.")
	}
	var b strings.Builder
	n := 0
	for _, r := range mine {
		if r.GuildID != req.GuildID {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n`%d` <t:%d:f> in <#%s>", r.ID, r.Next.Unix(), r.ChannelID)
		if !r.Rule.Once() {
			b.WriteString(", " + r.Rule.String())
		}
		if !r.Until.IsZero() {
			fmt.Fprintf(&b, ", until <t:%d:f>", r.Until.Unix())
		}
		b.WriteString(": " + oneLine(r.Text, 80))
	}
	if n == 0 {
		return privateReply(ctx, req, "You have no reminders in this server. Set one with `remindme <when> <what>`.")
	}
	head := fmt.Sprintf("**Your reminders here** (%d)", n)
	if other := len(mine) - n; other > 0 {
		head += fmt.Sprintf(", plus %d in other servers", other)
	}
	return privateReply(ctx, req, truncate(head+b.String()+"\nCancel one with `remindme cancel <number>`.", maxMessageLen))
}

func (c RemindMe) cancel(ctx context.Context, req Request, arg string) error {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil || id <= 0 {
		return privateReply(ctx, req, "Which one? Use the number from `remindme list`, e.g. `remindme cancel 12`.\n\n"+c.help())
	}
	mine, err := c.Store.UserReminders(ctx, req.AuthorID)
	if err != nil {
		c.logger().Error("remindme: couldn't read reminders", "user", req.AuthorID, "err", err)
		return privateReply(ctx, req, saveFailed)
	}
	for _, r := range mine {
		if r.ID != id {
			continue
		}
		if _, err := c.Store.DeleteReminder(ctx, id); err != nil {
			c.logger().Error("remindme: couldn't delete", "reminder", id, "err", err)
			return privateReply(ctx, req, saveFailed)
		}
		c.logger().Info("remindme: cancelled", "reminder", id, "user", req.AuthorID)
		return privateReply(ctx, req, fmt.Sprintf("Cancelled reminder `%d`.", id))
	}
	return privateReply(ctx, req, fmt.Sprintf("You don't have a reminder `%d`. See `remindme list`.", id))
}

func (c RemindMe) location() *time.Location {
	if c.Location != nil {
		return c.Location
	}
	return time.UTC
}

func (c RemindMe) maxPerUser() int {
	if c.MaxPerUser > 0 {
		return c.MaxPerUser
	}
	return DefaultMaxReminders
}

func (c RemindMe) maxChars() int {
	if c.MaxTextChars > 0 {
		return c.MaxTextChars
	}
	return DefaultReminderChars
}

func (c RemindMe) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c RemindMe) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// RemindFirer sends a due reminder (it's the scheduler's remind.FireFunc):
//   - Its owner must still have remindme in that server, or it's gone.
//   - With ask set up, the ask grant and allowance left, the model words it
//     (charged 1, like a plain answer); otherwise, or if that fails, it's the
//     owner's own text. remindme alone never costs anything.
//   - It pings only its owner; mentions in the text never ping anyone.
//   - A failed send is tried once more (a rare double post beats a lost
//     reminder); a deleted channel makes the reminder gone. Missing
//     permission is just a failed send: a repeat tries again next time.
type RemindFirer struct {
	Allowed func(ctx context.Context, user, guild snowflake.ID, command string) bool
	Ask     *Ask // nil = ask is off: always the owner's text
	Send    func(ctx context.Context, channelID snowflake.ID, r Reply) error
	Gone    func(error) bool // the channel was deleted
	Logger  *slog.Logger

	worded, plain atomic.Int64
}

// FireStats are the firer's counts since start, for debug.
type FireStats struct{ Worded, Plain int64 }

// Stats returns the counts since start.
func (f *RemindFirer) Stats() FireStats {
	return FireStats{Worded: f.worded.Load(), Plain: f.plain.Load()}
}

// Fire implements remind.FireFunc.
func (f *RemindFirer) Fire(ctx context.Context, r remind.Reminder, how remind.Firing) error {
	if !f.Allowed(ctx, r.UserID, r.GuildID, RemindCommand) {
		return fmt.Errorf("%w: no remindme access", remind.ErrGone)
	}
	text, worded := r.Text, false
	if f.Ask != nil && f.Allowed(ctx, r.UserID, r.GuildID, AskCommand) {
		if t, err := f.Ask.Remind(ctx, r.UserID, r.UserName, r.Text); err == nil {
			text, worded = t, true
		} else {
			f.logger().Debug("remindme: sending the plain text", "reminder", r.ID, "reason", err)
		}
	}
	if worded {
		f.worded.Add(1)
	} else {
		f.plain.Add(1)
	}
	content := Mention(r.UserID) + " " + text
	if how.Last {
		content += " (last one)"
	}
	if how.Late {
		content += " (late: I was offline when it was due)"
	}
	reply := Reply{Content: truncate(content, maxMessageLen), Mentions: []snowflake.ID{r.UserID}}
	err := f.Send(ctx, r.ChannelID, reply)
	if err != nil && !f.Gone(err) && ctx.Err() == nil {
		f.logger().Warn("remindme: re-sending", "reminder", r.ID, "err", err)
		err = f.Send(ctx, r.ChannelID, reply)
	}
	if err != nil && f.Gone(err) {
		return fmt.Errorf("%w: %v", remind.ErrGone, err)
	}
	return err
}

func (f *RemindFirer) logger() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.New(slog.DiscardHandler)
}
