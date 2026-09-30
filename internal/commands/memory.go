package commands

import (
	"cmp"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/llm"
)

// DefaultMemoryTokens caps a channel's remembered conversation, roughly in
// model tokens, so a few long answers can't make every later question costly.
const DefaultMemoryTokens = 4000

// ChannelMemory remembers each channel's recent ask conversation: questions
// and the bot's answers only (nothing else in the channel reaches the bot).
// It lives in RAM only: never logged or stored, gone on restart.
//
// Only the last MaxMessages messages newer than MaxAge are kept, within
// MaxTokens. When over a limit, the oldest half is dropped at once rather than
// one at a time: the start of the conversation then stays the same for a few
// questions, which lets providers' prompt caching keep working.
type ChannelMemory struct {
	MaxMessages int           // a question and its answer are two; 0 = remember nothing
	MaxAge      time.Duration // 0 = no age limit
	MaxTokens   int           // 0 = DefaultMemoryTokens
	Now         func() time.Time

	mu       sync.Mutex
	channels map[snowflake.ID][]exchange
	seq      int // numbers exchanges, so answer messages can be added once sent
}

// exchange is one question and its answer, kept together so they're
// remembered and forgotten as a pair.
type exchange struct {
	seq        int
	questionID snowflake.ID   // the asking message, if any (to honour deletes)
	answerIDs  []snowflake.ID // the answer's messages, once sent
	question   llm.Message
	answer     llm.Message
	at         time.Time // when the answer arrived
}

// History returns channelID's remembered conversation, oldest first.
func (m *ChannelMemory) History(channelID snowflake.ID) []llm.Message {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	xs := m.trim(channelID)
	out := make([]llm.Message, 0, 2*len(xs))
	for _, x := range xs {
		out = append(out, x.question, x.answer)
	}
	return out
}

// Add remembers a question (sent as message questionID; 0 for slash) and its
// answer. The returned number identifies the exchange for AddAnswerMessage.
func (m *ChannelMemory) Add(channelID, questionID snowflake.ID, question, answer llm.Message) int {
	if m == nil || m.MaxMessages <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.channels == nil {
		m.channels = make(map[snowflake.ID][]exchange)
	}
	m.seq++
	m.channels[channelID] = append(m.channels[channelID], exchange{seq: m.seq, questionID: questionID, question: question, answer: answer, at: m.now()})
	m.trim(channelID)
	return m.seq
}

// AddAnswerMessage records that exchange seq's answer was sent as messageID.
func (m *ChannelMemory) AddAnswerMessage(channelID snowflake.ID, seq int, messageID snowflake.ID) {
	if m == nil || seq == 0 || messageID == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	xs := m.channels[channelID]
	for i := range xs {
		if xs[i].seq == seq {
			xs[i].answerIDs = append(xs[i].answerIDs, messageID)
			return
		}
	}
}

// Forget clears channelID's memory and reports whether there was any.
func (m *ChannelMemory) Forget(channelID snowflake.ID) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	had := len(m.trim(channelID)) > 0
	delete(m.channels, channelID)
	return had
}

// MemoryStatus is one channel's memory, for the owner's debug command (sizes
// only, never content).
type MemoryStatus struct {
	ChannelID snowflake.ID
	Messages  int
	Oldest    time.Time
	Tokens    int // estimated
}

// Status reports every channel that remembers something, by channel ID.
func (m *ChannelMemory) Status() []MemoryStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MemoryStatus
	for id := range m.channels {
		xs := m.trim(id)
		if len(xs) == 0 {
			continue
		}
		out = append(out, MemoryStatus{ChannelID: id, Messages: 2 * len(xs), Oldest: xs[0].at, Tokens: tokens(xs)})
	}
	slices.SortFunc(out, func(a, b MemoryStatus) int { return cmp.Compare(a.ChannelID, b.ChannelID) })
	return out
}

// Has reports whether messageID is one of channelID's remembered questions or
// answers (so quoting it would repeat what the model already gets).
func (m *ChannelMemory) Has(channelID, messageID snowflake.ID) bool {
	if m == nil || messageID == 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.trim(channelID) {
		if x.questionID == messageID || slices.Contains(x.answerIDs, messageID) {
			return true
		}
	}
	return false
}

// ForgetMessage drops the exchange asked by messageID (deleted in Discord).
func (m *ChannelMemory) ForgetMessage(channelID, messageID snowflake.ID) {
	if m == nil || messageID == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	xs := m.channels[channelID]
	for i, x := range xs {
		if x.questionID == messageID {
			m.channels[channelID] = append(xs[:i:i], xs[i+1:]...)
			return
		}
	}
}

// trim applies the limits to channelID and returns what's left. Callers hold mu.
func (m *ChannelMemory) trim(channelID snowflake.ID) []exchange {
	xs := m.channels[channelID]
	if m.MaxAge > 0 {
		cutoff := m.now().Add(-m.MaxAge)
		i := 0
		for i < len(xs) && !xs[i].at.After(cutoff) {
			i++
		}
		xs = xs[i:]
	}
	maxExchanges := max(m.MaxMessages/2, 1)
	if m.MaxMessages <= 0 {
		maxExchanges = 0
	}
	limit := m.MaxTokens
	if limit <= 0 {
		limit = DefaultMemoryTokens
	}
	for len(xs) > maxExchanges || (len(xs) > 0 && tokens(xs) > limit) {
		xs = xs[(len(xs)+1)/2:] // drop the oldest half (at least one)
	}
	if len(xs) == 0 {
		delete(m.channels, channelID)
		return nil
	}
	m.channels[channelID] = xs
	return xs
}

// tokens estimates the model tokens of xs: about 4 characters per token, plus
// a little per message for names and formatting.
func tokens(xs []exchange) int {
	n := 0
	for _, x := range xs {
		n += 8 + (utf8.RuneCountInString(x.question.Name)+utf8.RuneCountInString(x.question.Content)+utf8.RuneCountInString(x.answer.Content))/4
	}
	return n
}

func (m *ChannelMemory) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}
