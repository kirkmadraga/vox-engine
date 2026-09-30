package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestAnswerLog(t *testing.T) {
	l := &AnswerLog{Max: 3}
	l.Add(1, 10)
	l.Add(1, 10) // again: no double entry
	l.Add(2, 20)
	if !l.Has(1, 10) || !l.Has(2, 20) || l.Has(2, 10) || l.Has(1, 0) {
		t.Error("Has mismatch (IDs are per channel; 0 is never an answer)")
	}
	l.Add(1, 11)
	l.Add(1, 12) // a 4th: the oldest (10) goes
	if l.Has(1, 10) || !l.Has(2, 20) || !l.Has(1, 12) {
		t.Error("past Max, only the oldest should be forgotten")
	}
	var none *AnswerLog
	none.Add(1, 1)
	if none.Has(1, 1) {
		t.Error("a nil log is empty")
	}
}

// idReplier reports an ID for each reply it "sends", like Discord does.
type idReplier struct {
	syncReplier
	next snowflake.ID
}

func (r *idReplier) Reply(ctx context.Context, rep Reply) error {
	r.mu.Lock()
	r.next++
	id := r.next
	r.mu.Unlock()
	if rep.Sent != nil && !rep.Private {
		rep.Sent(id)
	}
	return r.syncReplier.Reply(ctx, rep)
}

// Every part of an answer can be replied to; refusals and hints can't.
func TestAskRecordsItsAnswers(t *testing.T) {
	log := &AnswerLog{}
	c := &Ask{LLM: &fakeLLM{answer: strings.Repeat("word ", 500)}, Answers: log} // 2 parts
	rep := &idReplier{}
	c.Run(context.Background(), Request{ChannelID: 5, AuthorID: 8, Args: "hi", Reply: rep})
	c.Run(context.Background(), Request{ChannelID: 5, AuthorID: 8, Args: " ", Reply: rep}) // usage hint (id 3)
	if !log.Has(5, 1) || !log.Has(5, 2) || log.Has(5, 3) {
		t.Errorf("answers recorded wrong: 1=%v 2=%v hint=%v", log.Has(5, 1), log.Has(5, 2), log.Has(5, 3))
	}
}
