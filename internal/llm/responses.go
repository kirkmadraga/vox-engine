package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	_ Provider = (*Responses)(nil)
	_ Reporter = (*Responses)(nil)
)

// Responses talks to an OpenAI-style Responses API (POST {BaseURL}/responses),
// which xAI (Grok) and OpenAI both serve. Search tools run on the provider's
// side; the answer reports what they used.
type Responses struct {
	ProviderName string // for logs, e.g. "xai"
	BaseURL      string // e.g. https://api.x.ai/v1
	APIKey       string // never logged
	Model        string
	Instructions string // the system prompt
	// MaxOutputTokens caps the answer (reasoning included, on reasoning models).
	MaxOutputTokens int
	// ReasoningEffort ("low", "medium", "high"), sent only when set.
	ReasoningEffort string
	// WebSearch makes search tools available; they're offered only on
	// requests that allow search.
	WebSearch bool
	// ImageUnderstanding lets search look at images it finds (xAI's
	// enable_image_understanding; sent only with WebSearch).
	ImageUnderstanding bool
	// MaxTurns caps rounds of tool use per question (0 = provider default).
	MaxTurns int
	HTTP     *http.Client // nil = http.DefaultClient
	Logger   *slog.Logger // debug: token and tool counts only, never content

	mu    sync.Mutex
	stats Stats
}

// Stats are a provider's totals since the bot started (RAM only), for the
// owner's debug command. Counts only, never content.
type Stats struct {
	Answers, Errors int
	LastAnswer      time.Time
	LastTook        time.Duration
	LastErrorAt     time.Time
	LastError       string // short, without the key
	InputTokens     int
	CachedTokens    int
	OutputTokens    int
	ReasoningTokens int
	ToolCalls       map[string]int // e.g. "web_search_calls": 18
	SearchAnswers   int            // answers that searched
	ImageAnswers    int            // answers that viewed images while searching
}

// Status describes a provider and its totals.
type Status struct {
	Name, Model, BaseURL string
	WebSearch            bool
	ImageUnderstanding   bool
	MaxTurns             int
	MaxOutputTokens      int
	ReasoningEffort      string
	Stats                Stats
}

// Reporter is implemented by providers that can describe themselves.
type Reporter interface {
	Status() Status
}

// Status implements Reporter.
func (r *Responses) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats
	st.ToolCalls = maps.Clone(r.stats.ToolCalls)
	return Status{Name: r.ProviderName, Model: r.Model, BaseURL: r.BaseURL, WebSearch: r.WebSearch,
		ImageUnderstanding: r.ImageUnderstanding, MaxTurns: r.MaxTurns, MaxOutputTokens: r.MaxOutputTokens,
		ReasoningEffort: r.ReasoningEffort, Stats: st}
}

func (r *Responses) recordError(err error) {
	msg := shorten(redact(err.Error(), r.APIKey), 160)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Errors++
	r.stats.LastErrorAt = time.Now()
	r.stats.LastError = msg
}

func (r *Responses) recordAnswer(out response, a Answer, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &r.stats
	s.Answers++
	s.LastAnswer, s.LastTook = time.Now(), took
	u := out.Usage
	s.InputTokens += u.InputTokens
	s.CachedTokens += u.InputDetails.CachedTokens
	s.OutputTokens += u.OutputTokens
	s.ReasoningTokens += u.OutputDetails.ReasoningTokens
	for k, n := range out.toolCounts() {
		if s.ToolCalls == nil {
			s.ToolCalls = map[string]int{}
		}
		s.ToolCalls[k] += int(n)
	}
	if a.Usage.WebSearch {
		s.SearchAnswers++
	}
	if a.Usage.ViewedImages {
		s.ImageAnswers++
	}
}

// maxResponseBytes caps a provider response; an answer is a few KB.
const maxResponseBytes = 2 << 20

func (r *Responses) Name() string { return r.ProviderName }

func (r *Responses) Complete(ctx context.Context, q Request) (Answer, error) {
	start := time.Now()
	a, out, err := r.complete(ctx, q)
	if err != nil {
		r.recordError(err)
		return Answer{}, err
	}
	r.recordAnswer(out, a, time.Since(start))
	return a, nil
}

func (r *Responses) complete(ctx context.Context, q Request) (Answer, response, error) {
	body, err := json.Marshal(r.request(q))
	if err != nil {
		return Answer{}, response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.BaseURL, "/")+"/responses", bytes.NewReader(body))
	if err != nil {
		return Answer{}, response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	client := r.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Answer{}, response{}, fmt.Errorf("%s: %w", r.ProviderName, redactErr(err, r.APIKey))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Answer{}, response{}, fmt.Errorf("%s: reading the response: %w", r.ProviderName, err)
	}
	if len(raw) > maxResponseBytes {
		return Answer{}, response{}, fmt.Errorf("%s: response over %d bytes", r.ProviderName, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return Answer{}, response{}, fmt.Errorf("%s: HTTP %d: %s", r.ProviderName, resp.StatusCode, errorText(raw, r.APIKey))
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Answer{}, response{}, fmt.Errorf("%s: unreadable response: %w", r.ProviderName, err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return Answer{}, response{}, fmt.Errorf("%s: %s", r.ProviderName, redact(out.Error.Message, r.APIKey))
	}
	a := Answer{Text: out.text(), Usage: out.usage()}
	if r.Logger != nil {
		u := out.Usage
		// INFO, not debug: the bot's operator should always be able to see what
		// answers cost. Counts only, never content.
		r.Logger.Info("llm: usage", "provider", r.ProviderName, "model", r.Model, "status", out.Status,
			"input_tokens", u.InputTokens, "cached_tokens", u.InputDetails.CachedTokens,
			"output_tokens", u.OutputTokens, "reasoning_tokens", u.OutputDetails.ReasoningTokens,
			"sources", u.NumSourcesUsed, "tools", out.toolCounts(),
			"search", a.Usage.WebSearch, "images", a.Usage.ViewedImages)
	}
	return a, out, nil
}

// request builds the request body. Earlier turns come from channel memory;
// people's names go in front of their words, since several people share a
// channel's conversation.
func (r *Responses) request(q Request) map[string]any {
	input := make([]map[string]any, 0, len(q.Conversation))
	for _, m := range q.Conversation {
		switch {
		case m.Role == Assistant:
			input = append(input, map[string]any{"role": "assistant", "content": m.Content})
		case m.Image != nil:
			input = append(input, map[string]any{"role": "user", "content": []map[string]any{
				{"type": "input_text", "text": named(m)},
				{"type": "input_image", "image_url": m.Image.URL},
			}})
		default:
			input = append(input, map[string]any{"role": "user", "content": named(m)})
		}
	}
	req := map[string]any{
		"model": r.Model,
		"input": input,
		"store": false, // the provider shouldn't keep the conversation
	}
	if r.Instructions != "" {
		req["instructions"] = r.Instructions
	}
	if r.MaxOutputTokens > 0 {
		req["max_output_tokens"] = r.MaxOutputTokens
	}
	if r.ReasoningEffort != "" {
		req["reasoning"] = map[string]any{"effort": r.ReasoningEffort}
	}
	if r.WebSearch && q.Search { // no tools offered: the model can't search
		tool := map[string]any{"type": "web_search"}
		if r.ImageUnderstanding {
			tool["enable_image_understanding"] = true
		}
		req["tools"] = []any{tool}
		if r.MaxTurns > 0 {
			req["max_turns"] = r.MaxTurns
		}
	}
	return req
}

func named(m Message) string {
	if m.Name == "" {
		return m.Content
	}
	return m.Name + ": " + m.Content
}

// response is the part of a Responses API reply the bot reads.
type response struct {
	Status     string         `json:"status"`
	OutputText string         `json:"output_text"`
	Output     []outputItem   `json:"output"`
	Usage      responseUsage  `json:"usage"`
	ToolUsage  map[string]any `json:"server_side_tool_usage"`
	Error      *responseError `json:"error"`
}

type outputItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	NumSourcesUsed int            `json:"num_sources_used"`
	ToolDetails    map[string]any `json:"server_side_tool_usage_details"`
}

type responseError struct {
	Message string `json:"message"`
}

// text is the answer: the message items' output_text, else the convenience field.
func (r response) text() string {
	var b strings.Builder
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			}
		}
	}
	if b.Len() == 0 {
		return r.OutputText
	}
	return b.String()
}

// toolCounts merges the tool counts the provider reported, in either place.
func (r response) toolCounts() map[string]float64 {
	counts := map[string]float64{}
	for _, m := range []map[string]any{r.Usage.ToolDetails, r.ToolUsage} {
		for k, v := range m {
			if n, ok := v.(float64); ok && n > 0 {
				counts[strings.ToLower(k)] += n
			}
		}
	}
	return counts
}

// usage works out what the answer used. Providers report it differently (xAI
// counts tool calls in usage details, OpenAI lists web_search_call items), so
// every signal counts.
func (r response) usage() Usage {
	var u Usage
	for k := range r.toolCounts() {
		switch {
		case strings.Contains(k, "view_image"):
			u.ViewedImages = true
		case strings.Contains(k, "web_search"), strings.Contains(k, "image_search"),
			strings.Contains(k, "browse"), strings.Contains(k, "open_page"):
			u.WebSearch = true
		}
	}
	for _, item := range r.Output {
		switch {
		case strings.Contains(item.Type, "view_image"):
			u.ViewedImages = true
		case strings.Contains(item.Type, "web_search"):
			u.WebSearch = true
		}
	}
	if r.Usage.NumSourcesUsed > 0 {
		u.WebSearch = true
	}
	return u
}

// errorText is a short, key-free description of an error response.
func errorText(raw []byte, key string) string {
	var e struct {
		Error any `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && e.Error != nil {
		switch v := e.Error.(type) {
		case string:
			msg = v
		case map[string]any:
			if s, ok := v["message"].(string); ok {
				msg = s
			}
		}
	}
	return shorten(redact(msg, key), 300) // redact first: a cut could split the key
}

// shorten cuts s to at most n characters (never inside one), marking the cut.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// redact removes the API key from s, should a provider or library echo it.
func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}

func redactErr(err error, key string) error {
	if key != "" && strings.Contains(err.Error(), key) {
		return errors.New(redact(err.Error(), key))
	}
	return err
}
