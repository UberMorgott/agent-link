package node

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
)

// A node tells its peers, per shared area, whether a live session is attached
// and how it wakes, and whether its worker answers otherwise; the state follows
// session start, end and silent expiry, and ends when the peer goes offline.
func TestPresencePropagates(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, []string{"dev"}, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, []string{"dev"}, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	work, proj := t.TempDir(), t.TempDir()
	b.SetFolders(work, map[string]string{"dev": proj})
	b.SetAutoAnswer(true)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })

	q, err := a.SendRequest(SendRequest{To: "b", Area: "dev", Body: "dev question"})
	if err != nil {
		t.Fatal(err)
	}
	memberPresence := func() *AreaPresence {
		info, err := a.Chat(q.ChatID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range info.Members {
			if m.Name == "b" {
				return m.Presence
			}
		}
		return nil
	}
	sees := func(what, area string, want AreaPresence) {
		t.Helper()
		eventually(t, what, func() bool { p, ok := a.PeerPresence("b", area); return ok && p == want })
	}
	sees("no session, auto answer", "dev", AreaPresence{Area: "dev", AutoAnswer: true})
	if p := memberPresence(); p == nil || *p != (AreaPresence{Area: "dev", AutoAnswer: true}) {
		t.Fatalf("chat member presence %+v", p)
	}
	eventually(t, "b has the presence of a", func() bool { _, ok := b.PeerPresence("a", ""); return ok })

	if _, err := b.RegisterSession(SessionRequest{SessionID: "s1", Provider: "claude", Folder: proj, Wake: WakeRewake}); err != nil {
		t.Fatal(err)
	}
	sees("rewake session", "dev", AreaPresence{Area: "dev", Session: WakeRewake, AutoAnswer: true, Counts: AgentCounts{Claude: 1}})
	if p := memberPresence(); p == nil || p.Session != WakeRewake {
		t.Fatalf("chat member presence after register %+v", p)
	}
	if _, err := b.RegisterSession(SessionRequest{SessionID: "s2", Provider: "codex", Folder: work}); err != nil {
		t.Fatal(err)
	}
	sees("next-event session in the working folder", "", AreaPresence{Session: WakeNextEvent, AutoAnswer: true, Counts: AgentCounts{Codex: 1}})
	if err := b.EndSession("s1"); err != nil {
		t.Fatal(err)
	}
	sees("session ended", "dev", AreaPresence{Area: "dev", AutoAnswer: true})

	// A session that stops heartbeating expires without any call.
	if _, err := b.RegisterSession(SessionRequest{SessionID: "s3", Provider: "codex", Folder: proj, TTLSec: 1}); err != nil {
		t.Fatal(err)
	}
	sees("short session", "dev", AreaPresence{Area: "dev", Session: WakeNextEvent, AutoAnswer: true, Counts: AgentCounts{Codex: 1}})
	sees("short session expired", "dev", AreaPresence{Area: "dev", AutoAnswer: true})

	b.stop()
	eventually(t, "presence gone offline", func() bool { _, ok := a.PeerPresence("b", "dev"); return !ok && memberPresence() == nil })
}

// Members marks whose machine has an open agent session in the working folder
// (the project's): this node from its registry, a peer from its presence.
func TestMembersAgentFromPresence(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	workA, workB := t.TempDir(), t.TempDir()
	a.SetFolders(workA, nil)
	b.SetFolders(workB, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	agents := func(n *testNode) map[string]bool {
		out := map[string]bool{}
		for _, m := range n.Members() {
			out[m.Name] = m.Agent
		}
		return out
	}
	if got := agents(a); got["a"] || got["b"] {
		t.Fatalf("no sessions yet: %+v", got)
	}
	if _, err := a.RegisterSession(SessionRequest{SessionID: "own", Provider: "claude", Folder: workA}); err != nil {
		t.Fatal(err)
	}
	if got := agents(a); !got["a"] || got["b"] {
		t.Fatalf("own session: %+v", got)
	}
	if ms := a.Members(); ms[0].AgentCounts == nil || ms[0].AgentCounts.Claude != 1 {
		t.Fatalf("own agent counts: %+v", ms[0].AgentCounts)
	}
	if _, err := b.RegisterSession(SessionRequest{SessionID: "peer", Provider: "codex", Folder: workB}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a sees b's session", func() bool { return agents(a)["b"] })
	eventually(t, "a sees b's provider count", func() bool {
		for _, m := range a.Members() {
			if m.Name == "b" {
				return m.AgentCounts != nil && m.AgentCounts.Codex == 1
			}
		}
		return false
	})
	if err := b.EndSession("peer"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b's session ended", func() bool { return !agents(a)["b"] })
}

// A member's own chat color travels in its member record to every member; an
// unknown color is the default (""), and a record that lost it (an older
// peer re-wrote it) gets it back from its owner.
func TestChatColorPropagates(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	colorOf := func(n *testNode, name string) string {
		for _, m := range n.Members() {
			if m.Name == name {
				return m.Color
			}
		}
		return "?"
	}
	a.SetChatColor("violet")
	if got := colorOf(a, "a"); got != "violet" {
		t.Fatalf("own color %q", got)
	}
	eventually(t, "b sees a's color", func() bool { return colorOf(b, "a") == "violet" })
	a.SetChatColor("no-such-color")
	eventually(t, "b sees a's default color", func() bool { return colorOf(b, "a") == "" })
	a.SetChatColor("teal")
	eventually(t, "b sees teal", func() bool { return colorOf(b, "a") == "teal" })

	// An older peer's newer record of a without the color: a puts it back.
	a.mu.Lock()
	old := *a.members["a"]
	a.mu.Unlock()
	old.Color, old.Ver = "", old.Ver+1
	a.mergeMembers([]Member{old})
	a.mu.Lock()
	kept := a.members["a"].Color
	a.mu.Unlock()
	if kept != "teal" {
		t.Fatalf("own record after an old peer's copy: color %q", kept)
	}
	eventually(t, "b still sees teal", func() bool { return colorOf(b, "a") == "teal" })
}

// Only the areas both nodes have are told; the working folder's always is.
func TestPresenceSharedAreas(t *testing.T) {
	lnA := listen(t)
	a := newTestNode(t, "a", testSecret, []string{"dev", "ops"}, t.TempDir(), lnA, nil)
	got := a.presenceFor([]string{"ops", "web"})
	want := []AreaPresence{{Area: ""}, {Area: "ops"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("presence %+v, want %+v", got, want)
	}
}

func TestPresenceAgentCountsDeduplicatesSeats(t *testing.T) {
	p := newTestProject(t)
	n := newProjectNode(t, "a", p, listen(t), nil)
	folder := t.TempDir()
	n.SetFolders(folder, nil)
	for _, req := range []SessionRequest{
		{SessionID: "seat-codex", Provider: ProviderCodex, Folder: folder},
		{SessionID: "free-codex", Provider: ProviderCodex, Folder: folder},
	} {
		if _, err := n.RegisterSession(req); err != nil {
			t.Fatal(err)
		}
	}
	n.seats.mu.Lock()
	n.seats.seats = []*Seat{
		{ID: "seat-1", Provider: ProviderCodex, SessionID: "seat-codex"},
		{ID: "seat-2", Provider: ProviderClaude},
	}
	n.seats.run["seat-2"] = func() {}
	n.seats.mu.Unlock()

	got := n.presenceForCaps(nil, true)[0].Counts
	if got != (AgentCounts{Claude: 1, Codex: 2}) {
		t.Fatalf("counts include each active seat and free session once: %+v", got)
	}
	if old := n.presenceForCaps(nil, false)[0].Counts; old != (AgentCounts{}) {
		t.Fatalf("old peer received new counts: %+v", old)
	}
	oldWire, err := json.Marshal(n.presenceForCaps(nil, false))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(oldWire, []byte("agent_counts")) {
		t.Fatalf("old peer wire frame has agent counts: %s", oldWire)
	}
	n.seats.mu.Lock()
	n.seats.seats[0].Stopped = true
	n.seats.mu.Unlock()
	if got := n.presenceForCaps(nil, true)[0].Counts; got != (AgentCounts{Claude: 1, Codex: 1}) {
		t.Fatalf("stopped seat counted: %+v", got)
	}
	n.SetAutoAnswer(true)
	n.SetStopped(true)
	paused := n.presenceForCaps(nil, true)[0]
	if paused.Counts.total() != 0 || paused.Session != "" || paused.AutoAnswer {
		t.Fatalf("global pause advertised agents: %+v", paused)
	}
	n.SetStopped(false)
	resumed := n.presenceForCaps(nil, true)[0]
	if resumed.Counts != (AgentCounts{Claude: 1, Codex: 1}) || resumed.Session == "" || !resumed.AutoAnswer {
		t.Fatalf("resume did not advertise agents: %+v", resumed)
	}
}
