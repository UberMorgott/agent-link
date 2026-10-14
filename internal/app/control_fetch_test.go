package app

import (
	"encoding/json"
	"errors"
	"maps"
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
	app            *App
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
	f := &fetchSetup{t: t, srv: srv, dir: p.Dir, sid: "codex-one", app: alice}
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
// cut left out stays unread, and the same call made again takes it: a fetch
// has no cursor (no next; an `after` it is sent hides nothing, issue #41).
func TestFetchOverContextsDeliversWhatItShows(t *testing.T) {
	f := newFetchSetup(t)
	page, first := f.fetch(url.Values{"folder": {f.dir}, "limit": {"1"}})
	f.consistent(page, first)
	if len(first) != 1 || !page.Delivered || page.Next != "" || page.Note == "" {
		t.Fatalf("first call: %v delivered=%v next=%q note=%q", first, page.Delivered, page.Next, page.Note)
	}
	// With a cursor past every message, in the context the session is
	// registered in and in the one it is not.
	page, second := f.fetch(url.Values{"folder": {f.dir}, "limit": {"1"}, "after": {"~"}})
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

// fetchStore is three contexts' unread messages in cursor order (A: m1, B: m2,
// C: m3) behind deliverUnread, as controlUnread lists and delivers them;
// context B's delivery fails the first fails times it has something to deliver.
type fetchStore struct {
	unread map[string]string // id: its context
	fails  int
}

var errFetchBoom = errors.New("boom")

func newFetchStore(fails int) *fetchStore {
	return &fetchStore{unread: map[string]string{"m1": "A", "m2": "B", "m3": "C"}, fails: fails}
}

// call is one fetch: the merged listing after the cursor, delivered.
func (s *fetchStore) call(after string) (node.UnreadPage, error) {
	page := node.UnreadPage{Messages: []node.UnreadMessage{}}
	for _, id := range []string{"m1", "m2", "m3"} {
		if project, ok := s.unread[id]; ok && "c-"+id > after {
			m := node.UnreadMessage{Project: project, Cursor: "c-" + id}
			m.ID, m.Body = id, "body "+id
			page.Messages = append(page.Messages, m)
			page.Total++
		}
	}
	var ctxs []fetchCtx
	for _, project := range []string{"A", "B", "C"} {
		ctxs = append(ctxs, fetchCtx{id: project, deliver: func(_ string, ids []string) ([]string, error) {
			if project == "B" && len(ids) > 0 && s.fails > 0 {
				s.fails--
				return nil, errFetchBoom
			}
			for _, id := range ids {
				delete(s.unread, id)
			}
			return ids, nil
		}})
	}
	err := deliverUnread(ctxs, "codex-one", &page, 50, nil)
	return page, err
}

// The call a partly delivered fetch asks for returns the failed context's
// message (issue #41): following its answer (note: call again; a cursor when
// it gives one) until nothing more is said to wait delivers every message
// once. Before, next was the cursor of the last listed message, behind the
// one that was not delivered, and the call after it skipped that one.
func TestFetchFollowedAfterAContextFailed(t *testing.T) {
	s := newFetchStore(1)
	var got []string
	after := ""
	for call := 1; ; call++ {
		page, err := s.call(after)
		if fetchFailed(page, err) {
			t.Fatalf("call %d (after %q) failed: %v", call, after, err)
		}
		for _, m := range page.Messages {
			got = append(got, m.ID)
		}
		if page.Note == "" {
			break
		}
		if after = page.Next; call > 5 {
			t.Fatal("the fetch never ends")
		}
	}
	if want := []string{"m1", "m3", "m2"}; !slices.Equal(got, want) || len(s.unread) != 0 {
		t.Fatalf("delivered %v, want %v; still unread %v", got, want, s.unread)
	}
	// A context that keeps failing: the call the answer asks for is an error
	// that names it, not an empty answer that says nothing waits.
	s = newFetchStore(100)
	page, err := s.call("")
	if fetchFailed(page, err) || len(page.Messages) != 2 || page.Next != "" || !strings.Contains(page.Note, "не удалось") {
		t.Fatalf("the partly delivered fetch: %+v %v", page, err)
	}
	page, err = s.call(page.Next)
	if !fetchFailed(page, err) || !strings.Contains(err.Error(), "context B") {
		t.Fatalf("the call after it, with the context still failing: %+v %v", page, err)
	}
	if _, waits := s.unread["m2"]; !waits || len(s.unread) != 1 {
		t.Fatalf("unread after it: %v", s.unread)
	}
}

// A context whose delivery fails after another one acknowledged its messages
// does not turn the call into an error: the answer carries what was delivered
// (read now, never listed again), the failed context's message stays unread
// behind note (no cursor), and a context after the failed one is delivered too.
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
	err := deliverUnread([]fetchCtx{ok("A"), failing, ok("C")}, "codex-one", &page, 50, nil)
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
	if page.Next != "" || page.Note != node.FetchPartNote {
		t.Fatalf("nothing says the failed context's message still waits: next=%q note=%q", page.Next, page.Note)
	}
	if fetchFailed(page, err) {
		t.Fatal("a fetch delivered in part is answered as an error")
	}
	// Every context failed: nothing was delivered, and the call is an error,
	// not an empty answer that says nothing waits.
	page = node.UnreadPage{Messages: []node.UnreadMessage{msg("m2", "B")}, Total: 1}
	err = deliverUnread([]fetchCtx{failing}, "codex-one", &page, 50, nil)
	if !errors.Is(err, boom) || len(page.Messages) != 0 || page.Delivered || page.Note != "" {
		t.Fatalf("a fetch whose only context failed: %v %+v", err, page)
	}
	if !fetchFailed(page, err) {
		t.Fatal("a fetch that delivered nothing is answered as an empty page")
	}
	// Nothing waited and nothing failed: an empty answer.
	page = node.UnreadPage{}
	if err := deliverUnread([]fetchCtx{ok("A")}, "codex-one", &page, 50, nil); fetchFailed(page, err) {
		t.Fatalf("an empty fetch is an error: %v", err)
	}
}

// Issue #42 over two contexts, as controlUnread serves it: another session
// takes the network project's request between the fetch's listing and its
// delivery. The fetch passes over it and delivers the reply of the other
// context; the request is not acknowledged, and no answer asks for a call
// that would deliver nothing.
func TestFetchOverContextsPassesOverWhatAnotherOwnerTook(t *testing.T) {
	for _, limit := range []string{"", "1"} {
		t.Run("limit="+limit, func(t *testing.T) {
			f := newFetchSetup(t)
			other := node.SessionRequest{SessionID: "claude-two", Provider: node.ProviderClaude, Folder: f.dir, Wake: node.WakeRewake}
			if code, body := call(t, f.srv, http.MethodPost, "/sessions", jsonOf(t, other)); code != http.StatusOK {
				t.Fatalf("register: %d %s", code, body)
			}
			f.app.fetchListed = func() {
				f.app.fetchListed = nil
				if granted, err := f.net.Claim(node.ClaimRequest{SessionID: other.SessionID, IDs: []string{f.network}}); err != nil || len(granted) != 1 {
					t.Errorf("the other session's claim: %v %v", granted, err)
				}
			}
			q := url.Values{"folder": {f.dir}}
			if limit != "" {
				q.Set("limit", limit)
			}
			var got []string
			for n := 1; ; n++ {
				page, shown := f.fetch(maps.Clone(q))
				if page.Delivered != (len(shown) > 0) || page.Next != "" {
					t.Fatalf("call %d: %v delivered=%v next=%q", n, shown, page.Delivered, page.Next)
				}
				if n == 1 && !slices.Equal(shown, []string{f.reply}) {
					t.Fatalf("the first call: %v, want the reply %s (the request is another session's)", shown, f.reply)
				}
				got = append(got, shown...)
				if page.Note == "" {
					break
				}
				if limit == "" || n > 2 {
					t.Fatalf("call %d asks for another: %q", n, page.Note)
				}
			}
			if !slices.Equal(got, []string{f.reply}) {
				t.Fatalf("delivered %v, want the reply once", got)
			}
			if !f.unread(f.network) || f.unread(f.reply) {
				t.Fatalf("after the fetch: request unread=%v (want true), reply unread=%v (want false)", f.unread(f.network), f.unread(f.reply))
			}
		})
	}
}

// fillMsg is a listed message of context project whose text is part/10 of one
// answer's budget.
func fillMsg(id, project string, part int) node.UnreadMessage {
	m := node.UnreadMessage{Project: project, Cursor: "c-" + id}
	m.ID, m.Body = id, strings.Repeat("я", node.FormatBudget*part/10)
	return m
}

// fillCtxs are contexts A and B, which grant the ids of free and record what
// they were asked for and what they acknowledged.
func fillCtxs(free []string, asked, acked map[string][]string) []fetchCtx {
	var ctxs []fetchCtx
	for _, project := range []string{"A", "B"} {
		ctxs = append(ctxs, fetchCtx{id: project, deliver: func(_ string, ids []string) ([]string, error) {
			var granted []string
			for _, id := range ids {
				asked[project] = append(asked[project], id)
				if slices.Contains(free, id) {
					granted = append(granted, id)
				}
			}
			acked[project] = append(acked[project], granted...)
			return granted, nil
		}})
	}
	return ctxs
}

// The merged listing is [large m1 of A, m2 of B] and one answer carries m1
// alone; A grants it no more. The same call delivers m2 in B, acknowledges
// nothing in A and has no note. With nothing granted anywhere: an empty answer,
// no note, no error. A passed-over message takes no room of the answer.
func TestFetchPassesOverAcrossContexts(t *testing.T) {
	ids := func(p node.UnreadPage) []string {
		out := []string{}
		for _, m := range p.Messages {
			out = append(out, m.ID)
		}
		return out
	}
	listing := []node.UnreadMessage{fillMsg("m1", "A", 7), fillMsg("m2", "B", 7)}
	if node.FitUnread(listing) != 1 {
		t.Fatalf("%d of the listing fit one answer, want 1", node.FitUnread(listing))
	}
	asked, acked := map[string][]string{}, map[string][]string{}
	page := node.UnreadPage{Messages: slices.Clone(listing), Total: 2}
	err := deliverUnread(fillCtxs([]string{"m2"}, asked, acked), "codex-one", &page, 50, nil)
	if err != nil || !slices.Equal(ids(page), []string{"m2"}) || !page.Delivered || page.Note != "" || page.Next != "" || page.Total != 1 {
		t.Fatalf("the fetch: %v delivered=%v note=%q total=%d err=%v; want [m2] and no note", ids(page), page.Delivered, page.Note, page.Total, err)
	}
	if len(acked["A"]) != 0 || !slices.Equal(acked["B"], []string{"m2"}) || !slices.Equal(asked["A"], []string{"m1"}) || !slices.Equal(asked["B"], []string{"m2"}) {
		t.Fatalf("asked %v, acknowledged %v; want each id asked once and m2 alone acknowledged", asked, acked)
	}

	asked, acked = map[string][]string{}, map[string][]string{}
	page = node.UnreadPage{Messages: slices.Clone(listing), Total: 2}
	err = deliverUnread(fillCtxs(nil, asked, acked), "codex-one", &page, 50, nil)
	if err != nil || fetchFailed(page, err) || len(page.Messages) != 0 || page.Delivered || page.Note != "" || page.Next != "" {
		t.Fatalf("a fetch nothing of which is granted: %v note=%q %v; want an empty answer with no note", ids(page), page.Note, err)
	}
	if len(acked["A"])+len(acked["B"]) != 0 || !slices.Equal(asked["A"], []string{"m1"}) || !slices.Equal(asked["B"], []string{"m2"}) {
		t.Fatalf("asked %v, acknowledged %v", asked, acked)
	}

	// [m1 A, m2 B] fit and m3 does not; without m1 it does.
	listing = []node.UnreadMessage{fillMsg("m1", "A", 4), fillMsg("m2", "B", 4), fillMsg("m3", "A", 4)}
	if node.FitUnread(listing) != 2 || node.FitUnread(listing[1:]) != 2 {
		t.Fatalf("the listing fits %d, %d without the first; want 2 and 2", node.FitUnread(listing), node.FitUnread(listing[1:]))
	}
	asked, acked = map[string][]string{}, map[string][]string{}
	page = node.UnreadPage{Messages: slices.Clone(listing), Total: 3}
	err = deliverUnread(fillCtxs([]string{"m2", "m3"}, asked, acked), "codex-one", &page, 50, nil)
	if err != nil || !slices.Equal(ids(page), []string{"m2", "m3"}) || page.Note != "" {
		t.Fatalf("the fetch: %v note=%q err=%v; want [m2 m3] and no note", ids(page), page.Note, err)
	}
}

// Following the answers of a fetch over a merged listing of several pages, a
// part of which no context grants (the whole first page too): each claimable
// message is delivered once, none is asked for twice in a call, an answer with
// a note is never empty, and the last one has none. A context that fails
// meanwhile is reported and its messages come with a later call (issue #41).
func TestFetchFollowedOverAMixOfContexts(t *testing.T) {
	order := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"}
	project := map[string]string{"m1": "A", "m2": "B", "m3": "A", "m4": "B", "m5": "B", "m6": "A", "m7": "B"}
	taken := []string{"m1", "m2", "m4"} // another owner's, in both contexts
	unread := slices.Clone(order)
	fails := 1 // context B fails once, with something to deliver
	const limit = 2
	listing := func(after string) (node.UnreadPage, error) {
		page := node.UnreadPage{Messages: []node.UnreadMessage{}, Total: len(unread)}
		for _, id := range unread {
			if "c-"+id <= after {
				continue
			}
			if len(page.Messages) == limit {
				page.Next = page.Messages[limit-1].Cursor
				break
			}
			page.Messages = append(page.Messages, fillMsg(id, project[id], 1))
		}
		return page, nil
	}
	var got []string
	failed := 0
	for n := 1; ; n++ {
		asked := map[string]int{}
		var ctxs []fetchCtx
		for _, p := range []string{"A", "B"} {
			ctxs = append(ctxs, fetchCtx{id: p, deliver: func(_ string, ids []string) ([]string, error) {
				for _, id := range ids {
					asked[id]++
				}
				if p == "B" && fails > 0 && slices.ContainsFunc(ids, func(id string) bool { return !slices.Contains(taken, id) }) {
					fails--
					return nil, errFetchBoom
				}
				var granted []string
				for _, id := range ids {
					if !slices.Contains(taken, id) {
						granted = append(granted, id)
						unread = slices.DeleteFunc(unread, func(u string) bool { return u == id })
					}
				}
				return granted, nil
			}})
		}
		page, _ := listing("")
		err := deliverUnread(ctxs, "codex-one", &page, limit, listing)
		if err != nil {
			if failed++; !errors.Is(err, errFetchBoom) || !strings.Contains(err.Error(), "context B") || failed > 1 {
				t.Fatalf("call %d: %v", n, err)
			}
		}
		if fetchFailed(page, err) {
			t.Fatalf("call %d failed: %v", n, err)
		}
		for id, times := range asked {
			if times != 1 {
				t.Fatalf("call %d asked for %s %d times", n, id, times)
			}
		}
		if len(page.Messages) > limit || page.Next != "" || page.Delivered != (len(page.Messages) > 0) {
			t.Fatalf("call %d: %d messages, next=%q delivered=%v", n, len(page.Messages), page.Next, page.Delivered)
		}
		for _, m := range page.Messages {
			got = append(got, m.ID)
		}
		if page.Note == "" {
			break
		}
		if len(page.Messages) == 0 {
			t.Fatalf("call %d: a note with nothing delivered: the caller would spin", n)
		}
		if n > 6 {
			t.Fatal("the fetch never ends")
		}
	}
	slices.Sort(got)
	if want := []string{"m3", "m5", "m6", "m7"}; !slices.Equal(got, want) || failed != 1 {
		t.Fatalf("delivered %v, want each of %v once; %d calls reported a failed context, want 1", got, want, failed)
	}
	if !slices.Equal(unread, taken) {
		t.Fatalf("still unread: %v, want the taken ones %v", unread, taken)
	}
}
