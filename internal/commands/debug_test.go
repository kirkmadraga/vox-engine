package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access/accesstest"
	"github.com/kirkmadraga/vox-engine/internal/cache"
	"github.com/kirkmadraga/vox-engine/internal/llm"
	"github.com/kirkmadraga/vox-engine/internal/queue"
	"github.com/kirkmadraga/vox-engine/internal/remind"
	"github.com/kirkmadraga/vox-engine/internal/remind/remindtest"
	"github.com/kirkmadraga/vox-engine/internal/ytdlp"
)

// The owner's hidden status command: one subcommand per package, numbers and
// states only, never questions or answers.

var debugNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func runDebug(t *testing.T, d Debug, args string) string {
	t.Helper()
	rep := &fakeReplier{}
	if err := d.Run(context.Background(), Request{GuildID: 100, AuthorID: ownerID, Args: args, Reply: rep}); err != nil {
		t.Fatal(err)
	}
	if len(rep.got) != 1 || len(rep.got[0].Mentions) != 0 {
		t.Fatalf("%q: replies %+v, want one that pings no one", args, rep.got)
	}
	out := rep.got[0].Content
	if n := utf8.RuneCountInString(out); n > maxMessageLen {
		t.Errorf("%q: %d characters, over Discord's limit", args, n)
	}
	return out
}

func has(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, out)
		}
	}
}

func TestDebugHelp(t *testing.T) {
	for _, args := range []string{"", "  ", "nope", "LLM users"} {
		has(t, "help", runDebug(t, Debug{}, args), "`debug llm`", "`debug ask`", "`debug queue`", "`debug cache`", "`debug ytdlp`", "`debug access`")
	}
}

// Plain "debug" starts with the build, yt-dlp's version (asked each time) and uptime.
func TestDebugAbout(t *testing.T) {
	asked := 0
	d := Debug{Version: "v1.0.0 · commit 20e2e37 (2026-10-04 11:17 UTC) · Go 1.27.1 · disgo v0.19.6",
		YtdlpVersion: func(context.Context) (string, error) { asked++; return "2026.09.27", nil },
		Started:      debugNow.Add(-2*time.Hour - 14*time.Minute), Now: func() time.Time { return debugNow }}
	out := runDebug(t, d, "")
	want := "**vox-engine** v1.0.0 · commit 20e2e37 (2026-10-04 11:17 UTC) · Go 1.27.1 · disgo v0.19.6\nyt-dlp 2026.09.27 · up 2h 14m\n\n**debug** (owners only)"
	if !strings.HasPrefix(out, want) {
		t.Errorf("got:\n%s", out)
	}
	runDebug(t, d, "")
	if asked != 2 {
		t.Errorf("yt-dlp asked %d times for 2 debugs, want each time", asked)
	}
	d.YtdlpVersion = func(context.Context) (string, error) { return "", errors.New("not found") }
	has(t, "yt-dlp failing", runDebug(t, d, ""), "yt-dlp: couldn't ask for its version (not found) · up")
	d.YtdlpVersion = func(context.Context) (string, error) { return "2026.09.27", errors.New("timed out") }
	has(t, "yt-dlp failing again", runDebug(t, d, ""), "yt-dlp 2026.09.27 (couldn't check again: timed out) · up")
	long := strings.Repeat("x", 200) + "\nsecond line"
	d.YtdlpVersion = func(context.Context) (string, error) { return "", errors.New(long) }
	header, _, _ := strings.Cut(runDebug(t, d, ""), "\n\n")
	if lines := strings.Split(header, "\n"); len(lines) != 2 || !strings.Contains(lines[1], "x…) · up") {
		t.Errorf("a long error must stay on the header's line:\n%s", header)
	}
	has(t, "nothing set", runDebug(t, Debug{}, ""), "version unknown", "yt-dlp ?")
}

func TestDebugAccess(t *testing.T) {
	e := newEnv(accesstest.NewMemory())
	run(t, e.allow, "guild")
	has(t, "access", runDebug(t, Debug{Access: e.policy}, "Access"), "**Allowed servers** (1)", "`100` (this server)")
}

func TestDebugLLM(t *testing.T) {
	if out := runDebug(t, Debug{}, "llm"); !strings.Contains(out, "off") {
		t.Errorf("ask off: %s", out)
	}
	has(t, "echo", runDebug(t, Debug{LLM: llm.Echo{}}, "llm"), "`echo`", "stand-in")

	rep := fakeReporter{st: llm.Status{Name: "xai", Model: "grok-4.3", BaseURL: "https://api.x.ai/v1", WebSearch: true,
		ImageUnderstanding: true, MaxTurns: 1, MaxOutputTokens: 500, ReasoningEffort: "low",
		Stats: llm.Stats{Answers: 41, Errors: 2, LastAnswer: debugNow.Add(-2 * time.Minute), LastTook: 3100 * time.Millisecond,
			LastErrorAt: debugNow.Add(-41 * time.Minute), LastError: "xai: HTTP 429: slow down",
			InputTokens: 68210, CachedTokens: 4100, OutputTokens: 9870, ReasoningTokens: 7950,
			ToolCalls: map[string]int{"web_search_calls": 18}, SearchAnswers: 7, ImageAnswers: 3}}}
	d := Debug{LLM: rep, Ask: &Ask{LLM: rep, Search: SearchOnRequest}, Started: debugNow.Add(-3*time.Hour - 12*time.Minute), Now: func() time.Time { return debugNow }}
	has(t, "llm", runDebug(t, d, "llm"),
		"`xai` · `grok-4.3`", "Search: on-request (images on) · tool turns 1 · max tokens 500 · reasoning low",
		"Last answer: 2m 0s ago (took 3.1s) · last error 41m 0s ago: xai: HTTP 429: slow down",
		"Since start (3h 12m): 41 answers · 2 errors · 7 searched · 3 viewed images",
		"Tokens: in 68,210 (cached 4,100) · out 9,870 (thinking 7,950)", "Tool calls: web_search_calls 18",
		"\nReminders: worded by the same model")

	d.RemindLLM = fakeReporter{st: llm.Status{Name: "xai", Model: "grok-4.3-mini", ReasoningEffort: "none",
		Stats: llm.Stats{Answers: 12, Errors: 1, InputTokens: 3400, OutputTokens: 560}}}
	out := runDebug(t, d, "llm")
	has(t, "reminder model", out, "\nReminders: `grok-4.3-mini` · reasoning none · 12 worded · 1 errors · tokens in 3,400 · out 560 (thinking 0)")
	if strings.Contains(out, "same model") {
		t.Errorf("a reminder model set, but debug says the same: %s", out)
	}
}

type fakeReporter struct{ st llm.Status }

func (f fakeReporter) Name() string { return f.st.Name }
func (f fakeReporter) Complete(context.Context, llm.Request) (llm.Answer, error) {
	return llm.Answer{Text: "secret answer"}, nil
}
func (f fakeReporter) Status() llm.Status { return f.st }

func TestDebugAsk(t *testing.T) {
	if out := runDebug(t, Debug{}, "ask"); !strings.Contains(out, "off") {
		t.Errorf("ask off: %s", out)
	}
	clk := newClock()
	daily, _ := newDaily(10)
	daily.Now = clk.now
	mem := &ChannelMemory{MaxMessages: 10, Now: clk.now}
	a := &Ask{LLM: &fakeLLM{answer: "secret answer"}, Cooldown: 10 * time.Second, MaxConcurrent: 2, MaxPromptChars: 500,
		Timeout: time.Minute, Search: SearchOnRequest, Daily: daily, Memory: mem, Now: clk.now,
		IsOwner: func(id snowflake.ID) bool { return id == askOwner }}
	askWith(t, a, Request{ChannelID: 7, AuthorID: 8, Args: "secret question"})
	askWith(t, a, Request{ChannelID: 7, AuthorID: 8, Args: "too soon"}) // cooldown
	e := newEnv(accesstest.NewMemory())
	e.policy.Grant(context.Background(), 8, AskCommand, ownerID)  // has asked
	e.policy.Grant(context.Background(), 12, AskCommand, ownerID) // hasn't yet
	e.policy.Grant(context.Background(), 13, "play", ownerID)     // can't ask
	out := runDebug(t, Debug{Ask: a, Access: e.policy, Now: clk.now}, "ask")
	has(t, "ask", out,
		"search on-request · cooldown 10s · 2 at once · ≤500 chars · timeout 1m 0s",
		"Now: 0 of 2 slots busy · 0 waiting", "Since start: 1 answered · 0 failed · refused: cooldown 1",
		"Daily: 10/day (search 2, image 4)", "<@8>: balance 9/10 · cooldown 10s · 1 answers (0 searched) since start",
		"<#7>: 2 messages", "<@12>: balance 10/10 (hasn't asked today)")
	if strings.Contains(out, "<@13>") || strings.Count(out, "<@8>") != 1 {
		t.Errorf("people listed wrongly:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("debug ask shows content:\n%s", out)
	}
}

func TestDebugQueue(t *testing.T) {
	st := queue.Status{Slots: 2, SlotsUsed: 1, Waiting: 1, Guilds: []queue.GuildStatus{
		{GuildID: 100, Connected: true, Current: &queue.Track{Title: "Song", Duration: 225 * time.Second, VoiceChannel: 9, RequestedBy: 8}, Playing: 83 * time.Second, Queued: 3},
		{GuildID: 200, Connected: true, Idle: true},
		{GuildID: 300, Queued: 1},
		{GuildID: 400, Connected: true, Current: &queue.Track{URL: "https://youtu.be/x", VoiceChannel: 9, RequestedBy: 8}, Loading: true},
	}}
	has(t, "queue", runDebug(t, Debug{Queue: func() queue.Status { return st }}, "queue"),
		"voice slots 1 of 2 · 1 servers waiting",
		"server `100`: playing **Song** (1:23 / 3:45) in <#9>, requested by <@8> · 3 queued",
		"server `200`: idle in voice", "server `300`: waiting for a voice slot · 1 queued", "server `400`: loading")
	has(t, "empty queue", runDebug(t, Debug{Queue: func() queue.Status { return queue.Status{Slots: 1} }}, "queue"), "Nothing playing.")
}

func TestDebugCache(t *testing.T) {
	st := cache.Status{Bytes: 212 << 20, MaxBytes: 512 << 20, MaxAge: 12 * time.Hour, Songs: 38, OldestUse: debugNow.Add(-9 * time.Hour),
		Downloads: 3, Downloading: 1, DownloadSlots: 1, NextDownloadIn: 12 * time.Second}
	d := Debug{Cache: func(context.Context) (cache.Status, error) { return st, nil }, Now: func() time.Time { return debugNow }}
	has(t, "cache", runDebug(t, d, "cache"), "212 MB of 512 MB · 38 songs · least recently used 9h 0m ago · kept 12h 0m after last play",
		"Downloads: 1 running (of 1) · 2 waiting · next may start in 12s")
	d.Cache = func(context.Context) (cache.Status, error) { return cache.Status{}, errors.New("db") }
	has(t, "cache error", runDebug(t, d, "cache"), "couldn't read")
}

func TestDebugRemind(t *testing.T) {
	has(t, "off", runDebug(t, Debug{}, "remind"), "**remind**: off.")
	store := &remindtest.Memory{}
	store.AddReminder(context.Background(), remind.Reminder{UserID: 8, Text: "secret plans", Next: debugNow})
	store.AddReminder(context.Background(), remind.Reminder{UserID: 8, Text: "secret too", Rule: remind.Rule{Every: time.Hour}, Next: debugNow})
	d := Debug{Reminders: store, RemindZone: time.UTC, Started: debugNow.Add(-time.Hour), Now: func() time.Time { return debugNow },
		RemindStats: func() remind.Stats {
			return remind.Stats{Fired: 5, Late: 1, Dropped: 2, Gone: 1, NextDue: debugNow.Add(time.Hour)}
		},
		FireStats: func() FireStats { return FireStats{Worded: 3, Plain: 2} }}
	out := runDebug(t, d, "remind")
	has(t, "remind", out, "**remind**: 2 saved (1 repeat) · times in UTC", fmt.Sprintf("Next due <t:%d:R>", debugNow.Add(time.Hour).Unix()),
		"Since start (1h 0m): 5 sent (1 late) · 2 skipped (too late, or missed past their end) · 1 gone (deleted) · 0 failed", "Worded by the model 3 · own text 2")
	if strings.Contains(out, "secret") || strings.Contains(out, "<@8>") {
		t.Errorf("debug remind shows content or people:\n%s", out)
	}
}

func TestDebugYtdlp(t *testing.T) {
	d := Debug{Search: func() ytdlp.SearchStatus { return ytdlp.SearchStatus{Running: 1, Waiting: 2, Max: 1} }, Recent: &ytdlp.Recent{}}
	has(t, "ytdlp", runDebug(t, d, "ytdlp"), "searches: 1 running (of 1) · 2 waiting", "Last download: none yet", "Last search: none yet")
}

func TestDebugFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 3100 * time.Millisecond: "3.1s", 65 * time.Second: "1m 5s", 45 * time.Second: "45s",
		134 * time.Minute: "2h 14m", 50 * time.Hour: "2d 2h"} {
		if got := short(d); got != want {
			t.Errorf("short(%v) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 68210: "68,210", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := num(n); got != want {
			t.Errorf("num(%d) = %q, want %q", n, got, want)
		}
	}
	if position(83*time.Second) != "1:23" || position(3723*time.Second) != "1:02:03" {
		t.Error("position formatting")
	}
}
