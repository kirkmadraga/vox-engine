package commands

import (
	"context"
	"slices"
	"testing"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// Opt-in web search (the operator's choice, after measuring a search answer at
// 5-9x a plain one): with SearchOnRequest, only questions that start with
// "search" (or /ask search:True) may search; the rest are sent without search
// tools, so the model can't search.

func TestWantsSearch(t *testing.T) {
	cases := []struct {
		in, want string
		asked    bool
	}{
		{"search weather in lisbon", "weather in lisbon", true},
		{"Search   weather", "weather", true},
		{"search", "search", false}, // nothing to search for
		{"searching for x", "searching for x", false},
		{"research x", "research x", false},
		{"what to search", "what to search", false},
	}
	for _, c := range cases {
		if got, asked := wantsSearch(c.in); got != c.want || asked != c.asked {
			t.Errorf("wantsSearch(%q) = (%q, %v), want (%q, %v)", c.in, got, asked, c.want, c.asked)
		}
	}
}

// searchSeen runs one question and reports what the provider got.
func searchSeen(t *testing.T, mode, prompt string, slash bool) (question string, search bool) {
	t.Helper()
	llmc := newConvLLM()
	c := &Ask{LLM: llmc, Search: mode}
	askWith(t, c, Request{ChannelID: 1, Args: prompt, Search: slash})
	llmc.mu.Lock()
	defer llmc.mu.Unlock()
	conv := llmc.convs[len(llmc.convs)-1]
	return conv[len(conv)-1].Content, llmc.searches[len(llmc.searches)-1]
}

func TestAskSearchModes(t *testing.T) {
	cases := []struct {
		mode, prompt string
		slash        bool
		wantQ        string
		wantSearch   bool
	}{
		{SearchOnRequest, "weather in lisbon", false, "weather in lisbon", false},
		{SearchOnRequest, "search weather in lisbon", false, "[search requested] weather in lisbon", true},
		{SearchOnRequest, "weather in lisbon", true, "[search requested] weather in lisbon", true}, // /ask search:True
		{SearchOff, "search weather in lisbon", false, "search weather in lisbon", false},
		{SearchOff, "weather", true, "weather", false},
		{"", "search weather", false, "search weather", false}, // unset = off
		{SearchAlways, "weather in lisbon", false, "weather in lisbon", true},
		{SearchAlways, "search weather in lisbon", false, "weather in lisbon", true},
	}
	for _, c := range cases {
		q, s := searchSeen(t, c.mode, c.prompt, c.slash)
		if q != c.wantQ || s != c.wantSearch {
			t.Errorf("%s %q slash=%v: provider got (%q, search=%v), want (%q, %v)", c.mode, c.prompt, c.slash, q, s, c.wantQ, c.wantSearch)
		}
	}
}

// A requested search's cost is known up front: refused if the allowance
// can't cover it, without asking the model.
func TestAskSearchNeedsTheAllowance(t *testing.T) {
	llmc := newConvLLM()
	d, _ := newDaily(5)
	d.WeightSearch = 8
	c := &Ask{LLM: llmc, Daily: d, Search: SearchOnRequest}
	only(t, "5 left, search costs 8", askWith(t, c, Request{ChannelID: 1, Args: "search news"}), askDailyLimitSearch, true)
	only(t, "plain still fine", askWith(t, c, Request{ChannelID: 1, Args: "news"}), "re: news", false)
	if got := llmc.prompts(); !slices.Equal(got, []string{"news"}) {
		t.Errorf("the model got %q", got)
	}
	// With the model deciding (always), nothing is known up front.
	c.Search = SearchAlways
	only(t, "always: no pre-check", askWith(t, c, Request{ChannelID: 1, AuthorID: 9, Args: "news"}), "re: news", false)
}

// Charged by what happened: a search that wasn't needed costs 1.
func TestAskSearchChargedOnlyIfItSearched(t *testing.T) {
	d, _ := newDaily(20)
	d.WeightSearch = 8
	c := &Ask{LLM: &fakeLLM{answer: "ok"}, Daily: d, Search: SearchOnRequest}
	askWith(t, c, Request{ChannelID: 1, Args: "search what is 2+2"}) // model didn't search
	if b := balance(t, d, 8); b != 19 {
		t.Errorf("balance %d, want 19", b)
	}
	c.LLM = &fakeLLM{answer: "ok", usage: llm.Usage{WebSearch: true}}
	askWith(t, c, Request{ChannelID: 1, AuthorID: 8, Args: "search news"})
	if b := balance(t, d, 8); b != 11 {
		t.Errorf("balance %d, want 11", b)
	}
}

func TestAskRemembersTheQuestionWithoutTheSearchWord(t *testing.T) {
	mem := &ChannelMemory{MaxMessages: 10}
	c := &Ask{LLM: newConvLLM(), Memory: mem, Search: SearchOnRequest}
	askWith(t, c, Request{ChannelID: 1, Args: "search weather"})
	if h := mem.History(1); len(h) != 2 || h[0].Content != "weather" {
		t.Errorf("memory %+v", h)
	}
}

// Echo "searches" whenever it's allowed, so the weight shows in live tests.
func TestEchoSearchesWhenAllowed(t *testing.T) {
	got, _ := llm.Echo{}.Complete(context.Background(), llm.Request{Conversation: []llm.Message{{Content: "x"}}, Search: true})
	if !got.Usage.WebSearch {
		t.Error("echo should report a search when search is allowed")
	}
}

// The mark goes to the model only; memory keeps the plain question, and
// always-mode questions aren't marked (the model decides there).
func TestAskSearchMarkIsNotRemembered(t *testing.T) {
	mem := &ChannelMemory{MaxMessages: 10}
	llmc := newConvLLM()
	c := &Ask{LLM: llmc, Memory: mem, Search: SearchOnRequest}
	askWith(t, c, Request{ChannelID: 1, Args: "news", Search: true})
	if h := mem.History(1); len(h) != 2 || h[0].Content != "news" {
		t.Errorf("memory %+v", h)
	}
	if q, _ := searchSeen(t, SearchAlways, "news", false); q != "news" {
		t.Errorf("always mode sent %q", q)
	}
}

// A queued follow-up is checked again with the same up-front costs: the
// answer it waited behind may have left too little for a search.
func TestAskFollowUpRecheckedForSearchCost(t *testing.T) {
	llmc := newGateLLM("one")
	d, _ := newDaily(4)
	d.WeightSearch = 3
	c := &Ask{LLM: llmc, Daily: d, Search: SearchOnRequest}
	first := askAsync(context.Background(), c, 8, "one") // plain: 4 → 3 once answered
	<-llmc.started
	second := askAsync(context.Background(), c, 8, "search news") // needs 3: fine now…
	waitUntil(t, "the follow-up is waiting", func() bool { return c.isWaiting(8) })
	d.Store.SetDailyBalance(context.Background(), 8, d.day(), 3) // …but something else spends 1 meanwhile
	llmc.release("one")
	<-first
	only(t, "follow-up", <-second, askDailyLimitSearch, true)
	if got := llmc.prompts(); len(got) != 1 {
		t.Errorf("the model got %q; the follow-up must not reach it", got)
	}
}
