package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Replies to the bot's answers after a restart are recognised because only
// ask answers are sent as Discord replies (Reply.ReplyTo). If another command
// starts replying, replies to its messages would be taken for questions:
// this fails first, so that rule gets revisited.
func TestOnlyAskSendsReplies(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "ask.go" || f == "command.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "ReplyTo") {
			t.Errorf("%s sets Reply.ReplyTo: only ask's answers may be Discord replies (see router.replyToAnswer)", f)
		}
	}
}
