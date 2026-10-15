package node

import (
	"strings"
	"testing"
)

// Issue #40: `codex queue` keeps a prompt it accepted and runs it late, after
// its message was read and acknowledged inside another turn. The queued
// prompt must not carry that message: when it runs, it presents nothing the
// session already acknowledged.
func TestIssue40QueuedWakeCarriesNoAckedMessage(t *testing.T) {
	q := newStaleQueue(t)
	m := q.send("issue 40: request acknowledged in an earlier turn")
	q.pass()
	// The prompt as the thread gets it: without the test waker's
	// "[pointer for]" note of what a pointer was queued for.
	queued, _, _ := strings.Cut(q.lastWake(), "\n[pointer for] ")
	if queued == "" {
		t.Fatal("no prompt was queued")
	}
	// An earlier queued wake starts a turn; its `unread` returns m and it acks m.
	if _, err := q.a.Ack("", AckRequest{IDs: []string{m.ID}}); err != nil {
		t.Fatal(err)
	}
	// The prompt queued before the ack runs now, unchanged: it must not
	// present the acknowledged message as new work.
	if strings.Contains(queued, m.Body) || strings.Contains(queued, m.ID) {
		t.Fatalf("the queued prompt presents the acknowledged message %s when it runs late:\n%s", m.ID, queued)
	}
}
