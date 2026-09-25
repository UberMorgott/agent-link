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
