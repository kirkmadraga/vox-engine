package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// These tests run the client against a fake provider on this machine: what it
// sends, what it reads back, and how it fails. No API key or network needed.

const testKey = "xai-TESTKEY-0123456789"

// fakeProvider records the request and answers with status and body.
type fakeProvider struct {
	status int
	body   string
	delay  time.Duration

	gotPath, gotAuth, gotType string
	got                       map[string]any
}

func (f *fakeProvider) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath, f.gotAuth, f.gotType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		f.got = nil // record this request only
		json.Unmarshal(raw, &f.got)
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okBody(text string, usage string) string {
	return `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` +
		mustJSON(text) + `}]}],"usage":` + usage + `}`
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func client(url string) *Responses {
	return &Responses{ProviderName: "xai", BaseURL: url + "/v1/", APIKey: testKey, Model: "grok-4.3",
		Instructions: "Be brief.", MaxOutputTokens: 500, WebSearch: true, ImageUnderstanding: true, MaxTurns: 2}
}

var conv = []Message{
	{Role: User, Name: "Alice", Content: "pizza in naples?"},
	{Role: Assistant, Content: "Try Larsian."},
	{Role: User, Name: "Bob", Content: "what's this?", Image: &Image{URL: "https://cdn.discordapp.com/a.png", ContentType: "image/png"}},
}

func TestResponsesSendsTheRightRequest(t *testing.T) {
	f := &fakeProvider{status: 200, body: okBody("ok", `{}`)}
	srv := f.serve(t)
	c := client(srv.URL)
	c.ReasoningEffort = "low"
	if _, err := c.Complete(context.Background(), Request{Conversation: conv, Search: true}); err != nil {
		t.Fatal(err)
	}
	if f.gotPath != "/v1/responses" || f.gotAuth != "Bearer "+testKey || f.gotType != "application/json" {
		t.Errorf("path %q auth %q type %q", f.gotPath, f.gotAuth, f.gotType)
	}
	want := `{"input":[` +
		`{"content":"Alice: pizza in naples?","role":"user"},` +
		`{"content":"Try Larsian.","role":"assistant"},` +
		`{"content":[{"text":"Bob: what's this?","type":"input_text"},{"image_url":"https://cdn.discordapp.com/a.png","type":"input_image"}],"role":"user"}],` +
		`"instructions":"Be brief.","max_output_tokens":500,"max_turns":2,"model":"grok-4.3",` +
		`"reasoning":{"effort":"low"},"store":false,` +
		`"tools":[{"enable_image_understanding":true,"type":"web_search"}]}`
	if got := mustJSON(f.got); got != want {
		t.Errorf("request body:\n got  %s\n want %s", got, want)
	}
}

func TestResponsesOmitsWhatIsOff(t *testing.T) {
	f := &fakeProvider{status: 200, body: okBody("ok", `{}`)}
	c := client(f.serve(t).URL)
	c.WebSearch, c.Instructions, c.MaxOutputTokens = false, "", 0
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
	for _, k := range []string{"tools", "max_turns", "instructions", "max_output_tokens", "reasoning"} {
		if _, ok := f.got[k]; ok {
			t.Errorf("%q sent although off", k)
		}
	}
	if f.got["store"] != false {
		t.Error("store:false must always be sent")
	}
	// Image understanding needs search on; alone it sends nothing.
	c.ImageUnderstanding = true
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
	if _, ok := f.got["tools"]; ok {
		t.Error("tools sent without web search")
	}
}

func TestResponsesReadsTheAnswer(t *testing.T) {
	cases := map[string]string{
		okBody("Hello there.", `{}`): "Hello there.",
		`{"output":[{"type":"web_search_call"},{"type":"message","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}]}`: "ab",
		`{"output_text":"fallback field"}`: "fallback field",
		`{"output":[]}`:                    "",
	}
	for body, want := range cases {
		f := &fakeProvider{status: 200, body: body}
		got, err := client(f.serve(t).URL).Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
		if err != nil || got.Text != want {
			t.Errorf("%s: (%q, %v), want %q", body, got.Text, err, want)
		}
	}
}

// Providers report tool use differently; every form must count.
func TestResponsesReadsUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{
		{"nothing", okBody("a", `{"input_tokens":10,"output_tokens":5}`), Usage{}},
		{"xAI search count", okBody("a", `{"server_side_tool_usage_details":{"web_search_calls":2,"x_search_calls":0}}`), Usage{WebSearch: true}},
		{"xAI zero counts", okBody("a", `{"server_side_tool_usage_details":{"web_search_calls":0}}`), Usage{}},
		{"xAI SDK-style names", `{"output_text":"a","server_side_tool_usage":{"SERVER_SIDE_TOOL_WEB_SEARCH":1,"SERVER_SIDE_TOOL_VIEW_IMAGE":2}}`, Usage{WebSearch: true, ViewedImages: true}},
		{"view_image in details", okBody("a", `{"server_side_tool_usage_details":{"view_image_calls":1}}`), Usage{ViewedImages: true}},
		{"image search counts as search", `{"output_text":"a","server_side_tool_usage":{"SERVER_SIDE_TOOL_IMAGE_SEARCH":1}}`, Usage{WebSearch: true}},
		{"OpenAI search items", `{"output":[{"type":"web_search_call"},{"type":"message","content":[{"type":"output_text","text":"a"}]}]}`, Usage{WebSearch: true}},
		{"sources used", okBody("a", `{"num_sources_used":3}`), Usage{WebSearch: true}},
	}
	for _, c := range cases {
		f := &fakeProvider{status: 200, body: c.body}
		got, err := client(f.serve(t).URL).Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
		if err != nil || got.Usage != c.want {
			t.Errorf("%s: usage %+v (%v), want %+v", c.name, got.Usage, err, c.want)
		}
	}
}

func TestResponsesErrorsNeverShowTheKey(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":{"message":"Incorrect API key provided: ` + testKey + `"}}`, "HTTP 401"},
		{429, `{"error":"rate limited"}`, "rate limited"},
		{500, "upstream down", "HTTP 500: upstream down"},
		{200, "not json", "unreadable response"},
		{200, `{"error":{"message":"model not found"}}`, "model not found"},
	}
	for _, c := range cases {
		f := &fakeProvider{status: c.status, body: c.body}
		_, err := client(f.serve(t).URL).Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "TESTKEY") {
			t.Errorf("%d %s: err = %v, want %q and no key", c.status, c.body, err, c.want)
		}
	}
}

func TestResponsesCapsTheResponse(t *testing.T) {
	f := &fakeProvider{status: 200, body: `{"output_text":"` + strings.Repeat("x", maxResponseBytes) + `"}`}
	if _, err := client(f.serve(t).URL).Complete(context.Background(), Request{Conversation: conv[:1], Search: true}); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("err = %v, want a size error", err)
	}
}

func TestResponsesGivesUpWhenTheContextEnds(t *testing.T) {
	f := &fakeProvider{status: 200, body: okBody("late", `{}`), delay: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := client(f.serve(t).URL).Complete(ctx, Request{Conversation: conv[:1]}); err == nil {
		t.Error("want an error when the context ends")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("didn't give up promptly")
	}
}

// -debug shows token and tool counts, never content or the key.
func TestResponsesDebugLog(t *testing.T) {
	var buf bytes.Buffer
	f := &fakeProvider{status: 200, body: okBody("secret answer",
		`{"input_tokens":812,"output_tokens":120,"input_tokens_details":{"cached_tokens":600},"output_tokens_details":{"reasoning_tokens":40},"server_side_tool_usage_details":{"web_search_calls":1}}`)}
	c := client(f.serve(t).URL)
	c.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c.Complete(context.Background(), Request{Conversation: []Message{{Role: User, Name: "Alice", Content: "secret question"}}})
	out := buf.String()
	for _, want := range []string{"input_tokens=812", "cached_tokens=600", "output_tokens=120", "reasoning_tokens=40", "web_search_calls:1", "search=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "TESTKEY") || strings.Contains(out, "Alice") {
		t.Errorf("log leaks content or the key:\n%s", out)
	}
}

// No search tools unless the question may search: then the model can't.
func TestResponsesOffersSearchOnlyWhenAllowed(t *testing.T) {
	f := &fakeProvider{status: 200, body: okBody("ok", `{}`)}
	c := client(f.serve(t).URL) // web search on in config
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: false})
	for _, k := range []string{"tools", "max_turns"} {
		if _, ok := f.got[k]; ok {
			t.Errorf("%q sent for a question that didn't ask to search", k)
		}
	}
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
	if _, ok := f.got["tools"]; !ok {
		t.Error("tools missing for a search question")
	}
	c.WebSearch = false // search off in config: never, even if asked
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
	if _, ok := f.got["tools"]; ok {
		t.Error("tools sent with web search off")
	}
}

// The owner's debug command: totals since start, the last error without the key.
func TestResponsesStatus(t *testing.T) {
	ok := &fakeProvider{status: 200, body: okBody("a", `{"input_tokens":1000,"output_tokens":300,"input_tokens_details":{"cached_tokens":200},"output_tokens_details":{"reasoning_tokens":250},"server_side_tool_usage_details":{"web_search_calls":2}}`)}
	c := client(ok.serve(t).URL)
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
	c.Complete(context.Background(), Request{Conversation: conv[:1], Search: true})
	bad := &fakeProvider{status: 401, body: `{"error":{"message":"bad key ` + testKey + `"}}`}
	c.BaseURL = bad.serve(t).URL + "/v1"
	c.Complete(context.Background(), Request{Conversation: conv[:1]})

	st := c.Status()
	s := st.Stats
	if st.Name != "xai" || st.Model != "grok-4.3" || !st.WebSearch || st.MaxTurns != 2 {
		t.Errorf("settings: %+v", st)
	}
	if s.Answers != 2 || s.Errors != 1 || s.InputTokens != 2000 || s.CachedTokens != 400 || s.OutputTokens != 600 ||
		s.ReasoningTokens != 500 || s.ToolCalls["web_search_calls"] != 4 || s.SearchAnswers != 2 || s.LastAnswer.IsZero() {
		t.Errorf("stats: %+v", s)
	}
	if !strings.Contains(s.LastError, "HTTP 401") || strings.Contains(s.LastError, "TESTKEY") || s.LastErrorAt.IsZero() {
		t.Errorf("last error: %q", s.LastError)
	}
}

// A key straddling the cut point is removed before the cut, so no part of it
// survives (in the error, and in debug llm's last error).
func TestErrorsRedactBeforeCutting(t *testing.T) {
	msg := strings.Repeat("x", 290) + testKey + " more"
	if got := errorText([]byte(msg), testKey); strings.Contains(got, "xai-TEST") {
		t.Errorf("errorText leaks part of the key: %q", got[280:])
	}
	r := &Responses{APIKey: testKey}
	r.recordError(errors.New(strings.Repeat("y", 150) + testKey))
	if s := r.Status().Stats.LastError; strings.Contains(s, "xai-TEST") {
		t.Errorf("last error leaks part of the key: %q", s)
	}
}

// Cutting error text never splits a character.
func TestShorten(t *testing.T) {
	if got := shorten("héllo wörld", 4); got != "héll…" {
		t.Errorf("shorten = %q", got)
	}
	if got := shorten("short", 10); got != "short" {
		t.Errorf("shorten = %q", got)
	}
	if !utf8.ValidString(shorten(strings.Repeat("ñ", 400), 300)) {
		t.Error("cut inside a character")
	}
}
