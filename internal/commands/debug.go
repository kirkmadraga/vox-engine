package commands

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/cache"
	"github.com/kirkmadraga/vox-engine/internal/llm"
	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// DebugCommand is the owner-only "debug" command. It is deliberately hidden:
// mentions only (never registered as a slash command) and not in the README.
// Each subcommand is named after the package it reports on.
const DebugCommand = "debug"

// Debug is "@Bot debug [subcommand]": read-only status for the bot's owner.
// Numbers, names and states only; never questions, answers or other content.
type Debug struct {
	Access  AccessManager
	LLM     llm.Provider // nil when ask is off
	Ask     *Ask         // nil when ask is off
	Queue   func() queue.Status
	Cache   func(context.Context) (cache.Status, error)
	Search  func() ytdlp.SearchStatus
	Recent  *ytdlp.Recent
	Started time.Time
	Now     func() time.Time // nil = time.Now
}

func (Debug) Name() string { return DebugCommand }

const debugHelp = "**debug** (owners only)\n" +
	"`debug llm`: model bridge, tokens and tool calls since start\n" +
	"`debug ask`: limits, allowances, queues, cooldowns, channel memory\n" +
	"`debug queue`: voice slots, what's playing and queued in each server\n" +
	"`debug cache`: audio cache and downloads\n" +
	"`debug ytdlp`: searches, last download and search\n" +
	"`debug access`: allowed servers, grants and ask channels"

func (c Debug) Run(ctx context.Context, req Request) error {
	var out string
	switch sub := strings.ToLower(strings.TrimSpace(req.Args)); sub {
	case "llm":
		out = c.llm()
	case "ask":
		out = c.ask(ctx)
	case "queue":
		out = c.queue()
	case "cache":
		out = c.cache(ctx)
	case "ytdlp":
		out = c.ytdlp()
	case "access":
		snap, err := c.Access.Snapshot(ctx)
		if err != nil {
			out = "Couldn't read the access lists. Details are in the bot's log."
		} else {
			out = FormatAccess(snap, req.GuildID)
		}
	default:
		out = debugHelp
	}
	// No Mentions: user mentions render as names but never ping.
	return req.Reply.Reply(ctx, Reply{Content: truncate(out, maxMessageLen)})
}

func (c Debug) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c Debug) llm() string {
	if c.LLM == nil {
		return "**llm**: off (`llm_provider` isn't set)."
	}
	r, ok := c.LLM.(llm.Reporter)
	if !ok {
		return fmt.Sprintf("**llm**: `%s` (a stand-in; no usage to report)", c.LLM.Name())
	}
	st := r.Status()
	s := st.Stats
	var b strings.Builder
	fmt.Fprintf(&b, "**llm**: `%s` · `%s` · %s\n", st.Name, st.Model, st.BaseURL)
	search := "off"
	if st.WebSearch {
		search = "on"
		if c.Ask != nil {
			search = c.Ask.Status().Search
		}
		if st.ImageUnderstanding {
			search += " (images on)"
		}
	}
	fmt.Fprintf(&b, "Search: %s · tool turns %d · max tokens %d · reasoning %s\n",
		search, st.MaxTurns, st.MaxOutputTokens, orDash(st.ReasoningEffort))
	last := "none yet"
	if !s.LastAnswer.IsZero() {
		last = fmt.Sprintf("%s ago (took %s)", short(c.now().Sub(s.LastAnswer)), short(s.LastTook))
	}
	fmt.Fprintf(&b, "Last answer: %s", last)
	if !s.LastErrorAt.IsZero() {
		fmt.Fprintf(&b, " · last error %s ago: %s", short(c.now().Sub(s.LastErrorAt)), s.LastError)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Since start (%s): %d answers · %d errors · %d searched · %d viewed images\n",
		short(c.now().Sub(c.Started)), s.Answers, s.Errors, s.SearchAnswers, s.ImageAnswers)
	fmt.Fprintf(&b, "Tokens: in %s (cached %s) · out %s (thinking %s)",
		num(s.InputTokens), num(s.CachedTokens), num(s.OutputTokens), num(s.ReasoningTokens))
	if len(s.ToolCalls) > 0 {
		var calls []string
		for _, k := range slices.Sorted(maps.Keys(s.ToolCalls)) {
			calls = append(calls, fmt.Sprintf("%s %d", k, s.ToolCalls[k]))
		}
		fmt.Fprintf(&b, "\nTool calls: %s", strings.Join(calls, " · "))
	}
	return b.String()
}

func (c Debug) ask(ctx context.Context) string {
	if c.Ask == nil {
		return "**ask**: off (`llm_provider` isn't set)."
	}
	st := c.Ask.Status()
	var b strings.Builder
	fmt.Fprintf(&b, "**ask** · search %s · cooldown %s · %d at once · ≤%d chars · timeout %s\n",
		st.Search, short(st.Cooldown), st.MaxConcurrent, st.MaxPromptChars, short(st.Timeout))
	fmt.Fprintf(&b, "Now: %d of %d slots busy · %d waiting\n", st.SlotsBusy, st.MaxConcurrent, st.SlotsWaiting)
	fmt.Fprintf(&b, "Since start: %d answered · %d failed", st.Answered, st.Failed)
	if len(st.Refused) > 0 {
		var rs []string
		for _, k := range slices.Sorted(maps.Keys(st.Refused)) {
			rs = append(rs, fmt.Sprintf("%s %d", k, st.Refused[k]))
		}
		fmt.Fprintf(&b, " · refused: %s", strings.Join(rs, ", "))
	}
	b.WriteString("\n")

	balances := map[string]int{}
	d := c.Ask.Daily
	if d != nil && d.Limit > 0 {
		rows, next, err := d.Today(ctx)
		if err != nil {
			b.WriteString("Daily: couldn't read the balances (see the log)\n")
		} else {
			fmt.Fprintf(&b, "Daily: %d/day (search %d, image %d) · resets in %s\n",
				d.Limit, d.WeightSearch, d.WeightImage, short(next.Sub(c.now())))
			for _, r := range rows {
				balances[r.User.String()] = r.Balance
			}
		}
	}
	people := map[string]string{}
	var order []string
	for _, u := range st.Users {
		var parts []string
		if bal, ok := balances[u.ID.String()]; ok {
			parts = append(parts, fmt.Sprintf("balance %d/%d", bal, d.Limit))
			delete(balances, u.ID.String())
		}
		switch {
		case u.Running && u.Waiting:
			parts = append(parts, "running + 1 waiting")
		case u.Running:
			parts = append(parts, "running")
		}
		if u.CooldownLeft > 0 {
			parts = append(parts, "cooldown "+short(u.CooldownLeft))
		}
		if u.Answers > 0 {
			parts = append(parts, fmt.Sprintf("%d answers (%d searched) since start", u.Answers, u.Searches))
		}
		id := u.ID.String()
		order = append(order, id)
		people[id] = strings.Join(parts, " · ")
	}
	for id, bal := range balances { // asked before this start, not since
		order = append(order, id)
		people[id] = fmt.Sprintf("balance %d/%d", bal, d.Limit)
	}
	// Everyone who may ask, even if they haven't yet.
	if c.Access != nil {
		if snap, err := c.Access.Snapshot(ctx); err == nil {
			for _, g := range snap.Grants {
				id := g.UserID.String()
				if g.Command != AskCommand || people[id] != "" || slices.Contains(order, id) {
					continue
				}
				order = append(order, id)
				if d != nil && d.Limit > 0 {
					people[id] = fmt.Sprintf("balance %d/%d (hasn't asked today)", d.Limit, d.Limit)
				} else {
					people[id] = "hasn't asked yet"
				}
			}
		}
	}
	slices.Sort(order)
	if len(order) > 0 {
		b.WriteString("People:\n")
		for _, id := range order {
			fmt.Fprintf(&b, "- <@%s>: %s\n", id, orDash(people[id]))
		}
	}
	if mem := c.Ask.Memory.Status(); len(mem) > 0 {
		b.WriteString("Channel memory:\n")
		for _, m := range mem {
			fmt.Fprintf(&b, "- <#%s>: %d messages · oldest %s ago · ~%s tokens\n",
				m.ChannelID, m.Messages, short(c.now().Sub(m.Oldest)), num(m.Tokens))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (c Debug) queue() string {
	if c.Queue == nil {
		return "**queue**: not available."
	}
	st := c.Queue()
	var b strings.Builder
	fmt.Fprintf(&b, "**queue** · voice slots %d of %d · %d servers waiting", st.SlotsUsed, st.Slots, st.Waiting)
	if len(st.Guilds) == 0 {
		b.WriteString("\nNothing playing.")
	}
	for _, g := range st.Guilds {
		fmt.Fprintf(&b, "\n- server `%s`: ", g.GuildID)
		switch {
		case g.Current != nil && g.Loading:
			fmt.Fprintf(&b, "loading %s", g.Current.Name())
		case g.Current != nil:
			fmt.Fprintf(&b, "playing %s (%s", g.Current.Name(), position(g.Playing))
			if g.Current.Duration > 0 {
				fmt.Fprintf(&b, " / %s", position(g.Current.Duration))
			}
			b.WriteString(")")
		case g.Idle:
			b.WriteString("idle in voice")
		case g.Connected:
			b.WriteString("in voice")
		default:
			b.WriteString("waiting for a voice slot")
		}
		if g.Current != nil {
			fmt.Fprintf(&b, " in <#%s>, requested by <@%s>", g.Current.VoiceChannel, g.Current.RequestedBy)
		}
		if g.Queued > 0 {
			fmt.Fprintf(&b, " · %d queued", g.Queued)
		}
	}
	return b.String()
}

func (c Debug) cache(ctx context.Context) string {
	if c.Cache == nil {
		return "**cache**: not available."
	}
	st, err := c.Cache(ctx)
	if err != nil {
		return "**cache**: couldn't read the index (see the log)."
	}
	var b strings.Builder
	limit := "no size cap"
	if st.MaxBytes > 0 {
		limit = "of " + mb(st.MaxBytes)
	}
	fmt.Fprintf(&b, "**cache** · %s %s · %d songs", mb(st.Bytes), limit, st.Songs)
	if !st.OldestUse.IsZero() {
		fmt.Fprintf(&b, " · least recently used %s ago", short(c.now().Sub(st.OldestUse)))
	}
	if st.MaxAge > 0 {
		fmt.Fprintf(&b, " · kept %s after last play", short(st.MaxAge))
	}
	fmt.Fprintf(&b, "\nDownloads: %d running (of %d) · %d waiting", st.Downloading, st.DownloadSlots, max(st.Downloads-st.Downloading, 0))
	if st.NextDownloadIn > 0 {
		fmt.Fprintf(&b, " · next may start in %s", short(st.NextDownloadIn))
	}
	return b.String()
}

func (c Debug) ytdlp() string {
	var b strings.Builder
	b.WriteString("**ytdlp**")
	if c.Search != nil {
		s := c.Search()
		fmt.Fprintf(&b, " · searches: %d running (of %d) · %d waiting", s.Running, s.Max, s.Waiting)
	}
	dl, se := c.Recent.Last()
	run := func(label string, r ytdlp.Run, extract bool) {
		fmt.Fprintf(&b, "\n%s: ", label)
		if r.At.IsZero() {
			b.WriteString("none yet")
			return
		}
		fmt.Fprintf(&b, "%s ago · %s total", short(c.now().Sub(r.At)), short(r.Took))
		if extract && r.Extract > 0 {
			fmt.Fprintf(&b, " (%s of it extracting)", short(r.Extract))
		}
		fmt.Fprintf(&b, " · %s", r.Outcome)
	}
	run("Last download", dl, true)
	run("Last search", se, false)
	return b.String()
}

// short formats d compactly: "2h 14m", "3m 5s", "4.9s", "0s".
func short(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < 10*time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		m, s := int(d/time.Minute), int(d%time.Minute/time.Second)
		if m == 0 {
			return fmt.Sprintf("%ds", s)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// position formats a playback position: "1:23", "1:02:03".
func position(d time.Duration) string {
	s := int(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// num groups thousands: 68210 → "68,210".
func num(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func mb(bytes int64) string { return fmt.Sprintf("%d MB", bytes>>20) }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
