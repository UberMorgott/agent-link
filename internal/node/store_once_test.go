package node

import (
	"os"
	"path/filepath"
	"testing"
)

// entryOf is the Inbox entry of message id in direction dir.
func entryOf(t *testing.T, n *testNode, id, dir string) (Entry, bool) {
	t.Helper()
	list, err := n.Inbox(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if e.ID == id && e.Direction == dir {
			return e, true
		}
	}
	return Entry{}, false
}

// A chat message's body is stored once, in the chat store: the receiver's
// inbox record and the sender's sent copy keep its identity alone, and the
// history views still show it whole.
func TestChatMessageStoredOnce(t *testing.T) {
	a, b := deliveryPair(t, t.TempDir(), nil, nil)
	m := ask(t, a, b, "the body lives in the chat store")

	var in inboxRecord
	if err := readJSON(a.store.inboxPath(m.ID), &in); err != nil || !in.Slim || in.Message.Body != "" || in.Message.ChatID != m.ChatID {
		t.Fatalf("inbox record %+v %v", in, err)
	}
	if rec, ok := a.chats.message(m.ID); !ok || rec.Message.Body != m.Body {
		t.Fatalf("chat record %+v", rec)
	}
	if e, ok := entryOf(t, a, m.ID, "in"); !ok || e.Body != m.Body {
		t.Fatalf("a's inbox entry %+v", e)
	}

	sent := filepath.Join(b.cfg.DataDir, "sent", "a", m.ID+".json")
	eventually(t, "b's copy acked", func() bool { _, err := os.Stat(sent); return err == nil })
	var out Message
	if err := readJSON(sent, &out); err != nil || out.Body != "" || out.ID != m.ID || out.To != "a" {
		t.Fatalf("sent copy %+v %v", out, err)
	}
	if b.store.delivery("a", m.ID) != "sent" {
		t.Fatal("no delivery evidence")
	}
	if e, ok := entryOf(t, b, m.ID, "out"); !ok || e.Body != m.Body || e.To != "a" || e.Status != "sent" {
		t.Fatalf("b's sent entry %+v", e)
	}
}

// A store written before (full inbox records and sent copies) loads and
// shows as before; a slim copy whose chat record is gone is left out.
func TestStoreLegacyFullCopies(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	full := Message{ID: newID(), From: "b", To: "a", ChatID: newID(), Body: "old full copy"}
	if err := writeJSON(st.inboxPath(full.ID), inboxRecord{Message: full}); err != nil {
		t.Fatal(err)
	}
	gone := Message{ID: newID(), From: "b", To: "a", ChatID: full.ChatID}
	if err := writeJSON(st.inboxPath(gone.ID), inboxRecord{Message: gone, Slim: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sent", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	own := Message{ID: newID(), From: "a", To: "b", ChatID: full.ChatID, Body: "old sent copy"}
	if err := writeJSON(filepath.Join(dir, "sent", "b", own.ID+".json"), own); err != nil {
		t.Fatal(err)
	}
	st, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.body = func(string) (Message, bool) { return Message{}, false }
	list, err := st.recent(0, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range list {
		got[e.ID] = e.Body
	}
	if len(got) != 2 || got[full.ID] != full.Body || got[own.ID] != own.Body {
		t.Fatalf("recent %+v", got)
	}
}
