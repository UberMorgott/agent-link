package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// askCodex adds the seats, asks the Codex seat in a new chat and runs its turn
// with during; it returns the node, the Codex seat, the chat and the ask.
func askCodex(t *testing.T, during func(a *testNode, spec LaunchSpec)) (*testNode, SeatView, string, Message) {
	t.Helper()
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	l.set(nil, func(spec LaunchSpec) { during(a, spec) })
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "the code word?", AuthorKind: AuthorAgent, AskSeats: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "codex turn ran", func() bool {
		s := seatByLabel(t, a, "Codex")
		return s.Status != SeatRunning && (len(s.Pending) == 0 || s.Fails > 0)
	})
	runs := l.all()
	if last := runs[len(runs)-1]; !strings.Contains(last.Prompt, "agent-link сам отправит его в чат ответом") || strings.Contains(last.Prompt, "--reply-to "+m.ID) {
		t.Fatalf("turn prompt %q", last.Prompt)
	}
	return a, codex, chat.ID, m
}

// replies are the replies to id in chat.
func replies(t *testing.T, a *testNode, chat, id string) []ChatMessage {
	t.Helper()
	page, err := a.ChatMessages(chat, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []ChatMessage
	for _, m := range page {
		if m.ReplyTo == id {
			out = append(out, m)
		}
	}
	return out
}

// A seat that answers in plain text and never runs agentlink send: the node
// posts its final answer as its reply, intact, and acknowledges the ask.
func TestSeatFinalAnswerPostedAsReply(t *testing.T) {
	a, codex, chat, m := askCodex(t, func(_ *testNode, spec LaunchSpec) { spec.Answer("  ZEBRA-7731 — done\n") })
	got := replies(t, a, chat, m.ID)
	if len(got) != 1 || got[0].Body != "ZEBRA-7731 — done" || got[0].Agent == nil || got[0].Agent.Seat != codex.ID || got[0].AuthorKind != AuthorAgent {
		t.Fatalf("replies %+v", got)
	}
	if l, ok := a.leases.get(leaseKey(codex.ID, m.ID)); !ok || l.State != LeaseAcked {
		t.Fatalf("lease %+v", l)
	}
	if s := seatByLabel(t, a, "Codex"); s.Fails != 0 || s.Error != "" {
		t.Fatalf("seat %+v", s)
	}
}

// A seat that replied itself (agentlink send --reply-to) in its turn gets no
// second copy from its final answer.
func TestSeatOwnReplyNotDoubled(t *testing.T) {
	a, _, chat, m := askCodex(t, func(a *testNode, spec LaunchSpec) {
		for _, s := range a.Seats() {
			if s.ID != spec.Seat {
				continue
			}
			for _, p := range s.Pending {
				rec, _ := a.chats.message(p.ID)
				if _, err := a.SendRequest(SendRequest{ChatID: rec.Message.ChatID, ReplyTo: p.ID, Body: "own reply", AuthorKind: AuthorAgent, Seat: spec.Seat}); err != nil {
					t.Error(err)
				}
			}
		}
		spec.Answer("final text")
	})
	if got := replies(t, a, chat, m.ID); len(got) != 1 || got[0].Body != "own reply" {
		t.Fatalf("replies %+v", got)
	}
}

// A turn that ends with no reply to its ask (no send, no final answer) is no
// plain acknowledgement: the ask stays pending, the turn counts as failed and
// a discuss waiting for it learns why at once.
func TestSeatTurnWithoutReplyFails(t *testing.T) {
	a, codex, chat, m := askCodex(t, func(_ *testNode, spec LaunchSpec) { spec.Answer("") })
	if got := replies(t, a, chat, m.ID); len(got) != 0 {
		t.Fatalf("replies %+v", got)
	}
	s := seatByLabel(t, a, "Codex")
	if len(s.Pending) != 1 || s.Fails != 1 || !strings.Contains(s.Error, ErrNoReply.Error()) {
		t.Fatalf("seat %+v", s)
	}
	if l, ok := a.leases.get(leaseKey(codex.ID, m.ID)); ok && l.State == LeaseAcked {
		t.Fatalf("lease acked %+v", l)
	}
	if st := a.SeatAsk(codex.ID, m.ID, time.Now().Add(10*time.Minute)); !st.Stuck || !strings.Contains(st.Error, ErrNoReply.Error()) {
		t.Fatalf("ask state %+v", st)
	}
}

// Codex's final answer: the final_answer item wins over later unphased text,
// commentary never counts, a model without phases gives its last message.
func TestCodexAnswer(t *testing.T) {
	item := func(thread, turn, typ, text string, phase any) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": typ, "text": text, "phase": phase}})
		return b
	}
	var a codexAnswer
	a.item(item("th", "tu", "agentMessage", "narration", "commentary"), "th", "tu")
	if a.text != "" {
		t.Fatalf("commentary taken: %+v", a)
	}
	a.item(item("th", "tu", "agentMessage", "legacy 1", nil), "th", "tu")
	a.item(item("th", "tu", "agentMessage", "legacy 2", nil), "th", "tu")
	if a.text != "legacy 2" {
		t.Fatalf("legacy %+v", a)
	}
	a.item(item("th", "tu", "agentMessage", "ANSWER — 1", "final_answer"), "th", "tu")
	a.item(item("th", "tu", "agentMessage", "after", nil), "th", "tu")
	a.item(item("th", "other", "agentMessage", "other turn", "final_answer"), "th", "tu")
	a.item(item("x", "tu", "agentMessage", "other thread", "final_answer"), "th", "tu")
	a.item(item("th", "tu", "commandExecution", "cmd", nil), "th", "tu")
	if a.text != "ANSWER — 1" {
		t.Fatalf("final %+v", a)
	}
}

// Claude's final answer is the text of its successful result event.
func TestClaudeStreamAnswer(t *testing.T) {
	in := `{"type":"system","subtype":"init","session_id":"s-1"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"done — ok","session_id":"s-1"}` + "\n"
	got := "unset"
	if _, err := readClaudeStream(strings.NewReader(in), func(string) {}, nil, nil, func(s string) { got = s }); err != nil || got != "done — ok" {
		t.Fatalf("answer %q %v", got, err)
	}
	got = "unset"
	in = `{"type":"system","subtype":"init","session_id":"s-1"}` + "\n" + `{"type":"result","subtype":"error","is_error":true,"result":"boom"}` + "\n"
	if _, err := readClaudeStream(strings.NewReader(in), func(string) {}, nil, nil, func(s string) { got = s }); err == nil || got != "unset" {
		t.Fatalf("failed turn answered %q %v", got, err)
	}
}
