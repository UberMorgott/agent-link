package node

import (
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
	sees("rewake session", "dev", AreaPresence{Area: "dev", Session: WakeRewake, AutoAnswer: true})
	if p := memberPresence(); p == nil || p.Session != WakeRewake {
		t.Fatalf("chat member presence after register %+v", p)
	}
	if _, err := b.RegisterSession(SessionRequest{SessionID: "s2", Provider: "codex", Folder: work}); err != nil {
		t.Fatal(err)
	}
	sees("next-event session in the working folder", "", AreaPresence{Session: WakeNextEvent, AutoAnswer: true})
	if err := b.EndSession("s1"); err != nil {
		t.Fatal(err)
	}
	sees("session ended", "dev", AreaPresence{Area: "dev", AutoAnswer: true})

	// A session that stops heartbeating expires without any call.
	if _, err := b.RegisterSession(SessionRequest{SessionID: "s3", Provider: "codex", Folder: proj, TTLSec: 1}); err != nil {
		t.Fatal(err)
	}
	sees("short session", "dev", AreaPresence{Area: "dev", Session: WakeNextEvent, AutoAnswer: true})
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
	if _, err := b.RegisterSession(SessionRequest{SessionID: "peer", Provider: "codex", Folder: workB}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a sees b's session", func() bool { return agents(a)["b"] })
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
