package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// classicBatchMsgs are a reply for subagent a1 and a message for the session.
func classicBatchMsgs() []node.UnreadMessage {
	var reply, plain node.UnreadMessage
	reply.ID, reply.From, reply.Body, reply.ChatID, reply.Cursor = "m-1", "peer", "the reply", "c-1", "0001"
	reply.ForAgent, reply.ForAgentType = "a1", "Explore"
	plain.ID, plain.From, plain.Body, plain.ChatID, plain.Cursor = "m-2", "peer", "a plain one", "c-1", "0002"
	return []node.UnreadMessage{reply, plain}
}

// v0672Batch is the text formatBatch of v0.6.72 made of the messages forMe
// kept (it worded them itself, before node.Render did): the head, each
// message as node.FormatUnread words it, and how many are left.
func v0672Batch(kept []node.UnreadMessage, total int, folder, sid string, stop bool) string {
	var t strings.Builder
	if stop {
		t.WriteString("Пока вы работали, пришли сообщения agent-link. Прочитайте их и, где просят ответа, ответьте; затем завершайте.\n")
	} else {
		fmt.Fprintf(&t, "agent-link: непрочитанные сообщения для этой папки (%d). Вся переписка остаётся в истории чата.\n", total)
	}
	for _, m := range kept {
		t.WriteString("\n")
		t.WriteString(node.FormatUnread(m))
	}
	if rest := total - len(kept); rest > 0 {
		fmt.Fprintf(&t, "\nЕщё %d непрочитанных придут со следующим событием. Прочитать сейчас: agentlink chat unread --folder %q --after %s ; прочитанные подтвердить: agentlink chat ack --ids <id,...> --session %s\n",
			rest, folder, kept[len(kept)-1].Cursor, sid)
	}
	return strings.TrimRight(t.String(), "\n")
}

// The shell hooks' delivery (collect: forMe, then formatBatch) of a session
// without the in-process module, Codex's too: forMe picks the run's messages
// and marks an ended subagent's reply, and formatBatch words exactly those,
// as v0.6.72 did. It must not pick and mark a second time (node.Render does
// that for the module, which sends the page as the node listed it): a live
// subagent's own reply read as an ended one's, an ended one's was headed twice.
func TestClassicBatchOfSubagentReplies(t *testing.T) {
	const folder, sid, total = `C:\work`, "s", 5
	note := node.EndedAgentNote("Explore", "a1")
	for _, c := range []struct {
		name  string
		h     *hookSession
		stop  bool
		ids   []string
		notes int
		total int
	}{
		{name: "a live subagent's own run", h: &hookSession{st: &hookState{}, agent: "a1"}, ids: []string{"m-1"}, total: total - 1},
		{name: "the main run, the subagent ended", h: &hookSession{st: &hookState{}}, ids: []string{"m-1", "m-2"}, notes: 1, total: total},
		{name: "the main run at a Stop, the subagent ended", h: &hookSession{st: &hookState{}}, stop: true, ids: []string{"m-1", "m-2"}, notes: 1, total: total},
	} {
		t.Run(c.name, func(t *testing.T) {
			page := c.h.forMe(node.UnreadPage{Messages: classicBatchMsgs(), Total: total})
			b := formatBatch(page, folder, sid, c.stop)
			if want := v0672Batch(page.Messages, c.total, folder, sid, c.stop); b.text != want {
				t.Errorf("the text differs from v0.6.72's:\n got %q\nwant %q", b.text, want)
			}
			if n := strings.Count(b.text, note); n != c.notes {
				t.Errorf("%q is in the text %d times, want %d:\n%s", note, n, c.notes, b.text)
			}
			if !strings.Contains(b.text, "\nthe reply\n") {
				t.Errorf("the reply is not in the text:\n%s", b.text)
			}
			if !slices.Equal(b.ids, c.ids) {
				t.Errorf("ids %v, want %v", b.ids, c.ids)
			}
		})
	}
	// The literal text of one case, as v0.6.72 printed it (the message's own
	// lines left to node.FormatUnread, whose date is the machine's zone).
	page := (&hookSession{st: &hookState{}, agent: "a1"}).forMe(node.UnreadPage{Messages: classicBatchMsgs(), Total: total})
	want := "agent-link: непрочитанные сообщения для этой папки (4). Вся переписка остаётся в истории чата.\n\n" +
		node.FormatUnread(classicBatchMsgs()[0]) +
		"\nЕщё 3 непрочитанных придут со следующим событием. Прочитать сейчас: agentlink chat unread --folder \"C:\\\\work\" --after 0001 ; прочитанные подтвердить: agentlink chat ack --ids <id,...> --session s"
	if got := formatBatch(page, folder, sid, false).text; got != want {
		t.Errorf("a live subagent's own reply:\n got %q\nwant %q", got, want)
	}
}
