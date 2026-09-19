package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

func errorText(t *testing.T, body string) string {
	t.Helper()
	var r struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("not an error answer: %s", body)
	}
	return r.Error
}

func TestParticipantsExcludeSelfAndPreserveDetailsAndCounts(t *testing.T) {
	seen := time.Unix(1_750_000_000, 0).UTC()
	status := Status{Members: []node.MemberInfo{
		{Name: "alice", Self: true, Online: true, Addrs: []string{"127.0.0.1:7420"}},
		{Name: "bob", Online: false, Seen: seen, App: "v0.9.0", Addrs: []string{"10.0.0.2:7420"}},
	}}
	entries := []node.Entry{
		entry("out", strings.Repeat("a", 32), "alice", "bob", "one"),
		entry("out", strings.Repeat("b", 32), "alice", "bob", "two"),
		entry("in", strings.Repeat("c", 32), "bob", "alice", "three"),
	}

	got := buildParticipants(status, entries)
	if len(got) != 1 {
		t.Fatalf("participants = %+v, want one remote member", got)
	}
	p := got[0]
	if p.Self || p.Name != "bob" || p.Online || !p.Seen.Equal(seen) || p.App != "v0.9.0" ||
		len(p.Addrs) != 1 || p.Addrs[0] != "10.0.0.2:7420" || p.Sent != 2 || p.Received != 1 || p.Total != 3 {
		t.Fatalf("participant fields/counts changed: %+v", p)
	}
}

// «Добавить» keeps the address and connects; «Удалить» removes the member
// everywhere and drops its address from the settings.
func TestMemberButtons(t *testing.T) {
	a, b := newHarness(t), newHarness(t)
	if code, body := a.do(t, http.MethodPost, "/ui/api/members/add", `{"addr":"127.0.0.1:1"}`, a.tokenHdr()); code != http.StatusBadRequest ||
		errorText(t, body) != uiStrings["error.not_configured"] {
		t.Fatalf("add before the first save: %d %s", code, body)
	}
	for _, h := range []*harness{a, b} {
		name := map[*harness]string{a: "alice", b: "bob"}[h]
		body := `{"node":"` + name + `","code":"K7Q2MX","listen":"127.0.0.1:0","handler":"none"}`
		if code, got := h.do(t, http.MethodPost, "/ui/api/settings", body, h.tokenHdr()); code != http.StatusOK || strings.Contains(got, "error") {
			t.Fatalf("save %s: %d %s", name, code, got)
		}
	}
	// The save starts the node; it learns its own address once it runs.
	waitFor(t, "bob's own address", func() bool {
		ms := b.app.Status().Members
		return len(ms) > 0 && len(ms[0].Addrs) > 0
	})
	self := b.app.Status().Members[0]
	if !self.Self || self.Name != "bob" || len(self.Addrs) != 1 {
		t.Fatalf("bob's own entry %+v", self)
	}
	if code, body := a.do(t, http.MethodPost, "/ui/api/members/add", `{"addr":"10.0.0.1:99999"}`, a.tokenHdr()); code != http.StatusBadRequest ||
		errorText(t, body) != uiStrings["error.peer_addr"] {
		t.Fatalf("bad address: %d %s", code, body)
	}
	if code, body := a.do(t, http.MethodPost, "/ui/api/members/add", `{"addr":"`+self.Addrs[0]+`"}`, a.tokenHdr()); code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	waitFor(t, "alice and bob connected", func() bool { return a.app.Status().Connected && b.app.Status().Connected })
	st := a.app.Status()
	if st.Peer != "bob" || st.Online != 1 || st.Total != 1 || !strings.Contains(st.Summary(), "bob") {
		t.Fatalf("alice status %+v, summary %q", st, st.Summary())
	}
	if s, _, _ := settings.Load(a.path); len(s.Peers) != 1 || s.Peers[0].Addr != self.Addrs[0] {
		t.Fatalf("alice kept peers %+v", s.Peers)
	}
	// A later «Сохранить» (no peers in the form) keeps the added address.
	if code, got := a.do(t, http.MethodPost, "/ui/api/settings", `{"node":"alice","code":"K7Q2MX","listen":"127.0.0.1:0","handler":"none"}`, a.tokenHdr()); code != http.StatusOK || strings.Contains(got, "error") {
		t.Fatalf("resave: %d %s", code, got)
	}
	if s, _, _ := settings.Load(a.path); len(s.Peers) != 1 {
		t.Fatalf("resave dropped peers %+v", s.Peers)
	}
	waitFor(t, "reconnected after the save", func() bool { return a.app.Status().Connected })

	if code, body := a.do(t, http.MethodPost, "/ui/api/members/remove", `{"name":"alice"}`, a.tokenHdr()); code != http.StatusBadRequest ||
		errorText(t, body) != uiStrings["error.remove_self"] {
		t.Fatalf("remove self: %d %s", code, body)
	}
	if code, body := a.do(t, http.MethodPost, "/ui/api/members/remove", `{"name":"bob"}`, a.tokenHdr()); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, body)
	}
	waitFor(t, "bob gone", func() bool { st := a.app.Status(); return !st.Connected && st.Total == 0 })
	waitFor(t, "bob told", func() bool { return b.app.Status().Problem == "link.removed" })
	if s, _, _ := settings.Load(a.path); len(s.Peers) != 0 {
		t.Fatalf("removed member's address kept: %+v", s.Peers)
	}
}
