package commands

import (
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// DefaultAnswerLogSize is how many recent ask answers can be replied to.
const DefaultAnswerLogSize = 1000

// AnswerLog remembers the IDs of ask's recent answer messages, so a Discord
// reply to one of them can continue the conversation. RAM only; the oldest
// are forgotten past Max, and all on restart.
type AnswerLog struct {
	Max int // 0 = DefaultAnswerLogSize

	mu    sync.Mutex
	ids   map[answerKey]struct{}
	order []answerKey
}

type answerKey struct{ channel, message snowflake.ID }

// Add records messageID in channelID as an ask answer.
func (l *AnswerLog) Add(channelID, messageID snowflake.ID) {
	if l == nil || messageID == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids == nil {
		l.ids = make(map[answerKey]struct{})
	}
	k := answerKey{channelID, messageID}
	if _, ok := l.ids[k]; ok {
		return
	}
	l.ids[k] = struct{}{}
	l.order = append(l.order, k)
	limit := l.Max
	if limit <= 0 {
		limit = DefaultAnswerLogSize
	}
	for len(l.order) > limit {
		delete(l.ids, l.order[0])
		l.order = l.order[1:]
	}
}

// Has reports whether messageID in channelID is a recent ask answer.
func (l *AnswerLog) Has(channelID, messageID snowflake.ID) bool {
	if l == nil || messageID == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.ids[answerKey{channelID, messageID}]
	return ok
}
