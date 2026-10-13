package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// fetchSetup is a Codex session the node wakes by pointers, registered in a
// network project, with two messages that wait for it in two contexts: a
// request in the network project and the reply to its own discuss question
// in the folder's local chat, where the session is not registered.
type fetchSetup struct {
	t              *testing.T
	srv            *httptest.Server
	dir, sid       string
	net, local     *node.Node
	netID, localID string
	network, reply string
	done           []string // delivered by the fetches so far
}

func newFetchSetup(t *testing.T) *fetchSetup {
	t.Helper()
	p := newBinding(t, t.TempDir())
	alice, bob := pairAppsWith(t, func(a *App) { a.Launcher = &seatRunner{} }, p)
	srv := httptest.NewServer(alice.Handler())
	t.Cleanup(srv.Close)
	f := &fetchSetup{t: t, srv: srv, dir: p.Dir, sid: "codex-one"}
	session := node.SessionRequest{SessionID: f.sid, Provider: node.ProviderCodex, Folder: p.Dir, Wake: node.WakeQueue}
	if code, body := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, session)); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	code, body := call(t, srv, http.MethodPost, "/discuss", jsonOf(t, map[string]any{
		"folder": p.Dir, "provider": node.ProviderClaude, "source": node.ProviderCodex, "session_id": f.sid, "body": "question", "shared": true,
	}))
	var ask discussResultAPI
	if code != http.StatusOK || json.Unmarshal([]byte(body), &ask) != nil {
		t.Fatalf("discuss: %d %s", code, body)
	}
	f.local = alice.projects[ask.Project].n
	eventuallyApp(t, "seat setup", func() bool {
		for _, s := range f.local.Seats() {
			if s.SessionID == "" || s.LastTurn.IsZero() || s.Status == node.SeatRunning {
				return false
			}
		}
		return true
	})
	reply, err := f.local.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "answer", Seat: ask.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := bob.projects[p.ID].n.NewProjectChat([]string{"alice"})
	if err != nil {
		t.Fatal(err)
	}
	network, err := bob.projects[p.ID].n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: "Network question", Ask: []string{"alice"}, AuthorKind: node.AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	f.net, f.network, f.reply = alice.projects[p.ID].n, network.ID, reply.ID
	f.netID, f.localID = p.ID, ask.Project
	if _, registered := f.local.SessionLive(f.sid); registered {
		t.Fatal("the session is registered in the local chat too: this test proves nothing about a context it is not in")
	}
	eventuallyLong(t, "network question at alice", func() bool { return f.unread(f.network) })
	if !f.unread(f.reply) {
		t.Fatal("the discuss reply is not unread")
	}
	return f
}

// unread reports whether message id is still unread in its context.
func (f *fetchSetup) unread(id string) bool {
	n := f.net
	if id == f.reply {
		n = f.local
	}
	page, _ := n.Unread("", "", 100)
	return slices.ContainsFunc(page.Messages, func(m node.UnreadMessage) bool { return m.ID == id })
}

// fetch is the session's own unread listing (its MCP tool, its CLI command).
func (f *fetchSetup) fetch(q url.Values) (node.UnreadPage, []string) {
	f.t.Helper()
	q.Set("session", f.sid)
	q.Set("agent", "1")
	code, body := call(f.t, f.srv, http.MethodGet, "/unread?"+q.Encode(), "")
	var page node.UnreadPage
	if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
		f.t.Fatalf("fetch: %d %s", code, body)
	}
	var shown []string
	for _, m := range page.Messages {
		shown = append(shown, m.ID)
	}
	return page, shown
}

// consistent fails unless exactly the messages of a fetch's answer are read by
// it: none acknowledged that it does not show, none shown that stays unread,
// none shown twice.
func (f *fetchSetup) consistent(page node.UnreadPage, shown []string) {
	f.t.Helper()
	for _, id := range []string{f.network, f.reply} {
		in := slices.Contains(shown, id)
		if slices.Contains(f.done, id) {
			if in || f.unread(id) {
				f.t.Errorf("message %s, delivered before, is shown again or unread again", id)
			}
			continue
		}
		if !in && !f.unread(id) {
			f.t.Errorf("message %s was acknowledged by the fetch but is not in its answer %v", id, shown)
		}
		if in && f.unread(id) {
			f.t.Errorf("message %s is in the fetch's answer (delivered=%v) but stays unread", id, page.Delivered)
		}
	}
	if page.Delivered != (len(shown) > 0) {
		f.t.Errorf("delivered=%v with %v in the answer", page.Delivered, shown)
	}
	f.done = append(f.done, shown...)
}

// A queue session's fetch over two contexts delivers exactly what its answer
// shows: the listings are merged and cut first, then only the messages that
// are returned are claimed and acknowledged, each in its own context. What the
// cut left out stays unread behind next, and the next call takes it.
func TestFetchOverContextsDeliversWhatItShows(t *testing.T) {
	f := newFetchSetup(t)
	page, first := f.fetch(url.Values{"folder": {f.dir}, "limit": {"1"}})
	f.consistent(page, first)
	if len(first) != 1 || !page.Delivered || page.Next == "" || page.Note == "" {
		t.Fatalf("first call: %v delivered=%v next=%q note=%q", first, page.Delivered, page.Next, page.Note)
	}
	page, second := f.fetch(url.Values{"folder": {f.dir}, "limit": {"1"}})
	f.consistent(page, second)
	if len(second) != 1 || second[0] == first[0] || !page.Delivered || page.Next != "" || page.Note != "" {
		t.Fatalf("second call: %v after %v delivered=%v next=%q note=%q", second, first, page.Delivered, page.Next, page.Note)
	}
	if f.unread(f.network) || f.unread(f.reply) {
		t.Fatal("a message is still unread after both were delivered")
	}
	// Nothing repeats: not the request, not the reply of the context the
	// session is not registered in.
	for range 2 {
		if page, again := f.fetch(url.Values{"folder": {f.dir}}); len(again) != 0 || page.Delivered || page.Total != 0 {
			t.Fatalf("a delivered message is listed again: %v %+v", again, page)
		}
	}
}

// One call that carries both: both are read, the local chat's reply too
// (delivered in its own context, where the session is not registered), and
// the call that names no folder is of the session's own folder's contexts.
func TestFetchOverContextsWithoutFolder(t *testing.T) {
	f := newFetchSetup(t)
	page, shown := f.fetch(url.Values{"cwd": {t.TempDir()}}) // the pointer's CLI command, run from elsewhere
	f.consistent(page, shown)
	slices.Sort(shown)
	want := []string{f.network, f.reply}
	slices.Sort(want)
	if !slices.Equal(shown, want) || !page.Delivered || page.Next != "" || page.Note != "" {
		t.Fatalf("the fetch: %v, want %v; delivered=%v next=%q note=%q", shown, want, page.Delivered, page.Next, page.Note)
	}
	for _, m := range page.Messages {
		if want := map[bool]string{true: f.localID, false: f.netID}[m.ID == f.reply]; m.Project != want {
			t.Errorf("message %s names project %q, want %q", m.ID, m.Project, want)
		}
	}
	if page, again := f.fetch(url.Values{}); len(again) != 0 || page.Delivered {
		t.Fatalf("a delivered message is listed again: %v", again)
	}
}

// A context whose delivery fails after another one acknowledged its messages
// does not turn the call into an error: the answer carries what was delivered
// (read now, never listed again), the failed context's message stays unread
// behind note and next, and a context after the failed one is delivered too.
func TestFetchDeliversPartWhenAContextFails(t *testing.T) {
	msg := func(id, project string) node.UnreadMessage {
		m := node.UnreadMessage{Project: project, Cursor: "c-" + id}
		m.ID, m.Body = id, "body "+id
		return m
	}
	page := node.UnreadPage{Messages: []node.UnreadMessage{msg("m1", "A"), msg("m2", "B"), msg("m3", "C")}, Total: 3}
	acked := map[string][]string{}
	ok := func(project string) fetchCtx {
		return fetchCtx{id: project, deliver: func(session string, ids []string) ([]string, error) {
			acked[project] = append(acked[project], ids...)
			return ids, nil
		}}
	}
	boom := errors.New("boom")
	failing := fetchCtx{id: "B", deliver: func(string, []string) ([]string, error) { return nil, boom }}
	err := deliverUnread([]fetchCtx{ok("A"), failing, ok("C")}, "codex-one", &page)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "context B") {
		t.Fatalf("the failed context is not reported: %v", err)
	}
	var shown []string
	for _, m := range page.Messages {
		shown = append(shown, m.ID)
	}
	if !slices.Equal(shown, []string{"m1", "m3"}) || !page.Delivered || page.Total != 2 {
		t.Fatalf("the answer: %v delivered=%v total=%d, want the delivered part [m1 m3]", shown, page.Delivered, page.Total)
	}
	if !slices.Equal(acked["A"], []string{"m1"}) || !slices.Equal(acked["C"], []string{"m3"}) {
		t.Fatalf("acknowledged: %v", acked)
	}
	if page.Next == "" || page.Note == "" {
		t.Fatalf("nothing says the failed context's message still waits: next=%q note=%q", page.Next, page.Note)
	}
	if fetchFailed(page, err) {
		t.Fatal("a fetch delivered in part is answered as an error")
	}
	// Every context failed: nothing was delivered, and the call is an error,
	// not an empty answer that says nothing waits.
	page = node.UnreadPage{Messages: []node.UnreadMessage{msg("m2", "B")}, Total: 1}
	err = deliverUnread([]fetchCtx{failing}, "codex-one", &page)
	if !errors.Is(err, boom) || len(page.Messages) != 0 || page.Delivered || page.Note != "" {
		t.Fatalf("a fetch whose only context failed: %v %+v", err, page)
	}
	if !fetchFailed(page, err) {
		t.Fatal("a fetch that delivered nothing is answered as an empty page")
	}
	// Nothing waited and nothing failed: an empty answer.
	page = node.UnreadPage{}
	if err := deliverUnread([]fetchCtx{ok("A")}, "codex-one", &page); fetchFailed(page, err) {
		t.Fatalf("an empty fetch is an error: %v", err)
	}
}
