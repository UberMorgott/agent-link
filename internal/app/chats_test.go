package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// connectedPair saves settings on two apps and connects alice to bob.
func connectedPair(t *testing.T) (alice, bob *harness) {
	t.Helper()
	alice, bob = newHarness(t), newHarness(t)
	for _, h := range []*harness{alice, bob} {
		name := map[*harness]string{alice: "alice", bob: "bob"}[h]
		body := `{"node":"` + name + `","code":"K7Q2MX","listen":"127.0.0.1:0","handler":"none"}`
		if code, got := h.do(t, http.MethodPost, "/ui/api/settings", body, h.tokenHdr()); code != http.StatusOK || strings.Contains(got, "error") {
			t.Fatalf("save %s: %d %s", name, code, got)
		}
	}
	waitFor(t, "bob's own address", func() bool {
		ms := bob.app.Status().Members
		return len(ms) > 0 && len(ms[0].Addrs) > 0
	})
	addr := bob.app.Status().Members[0].Addrs[0]
	if code, body := alice.do(t, http.MethodPost, "/ui/api/members/add", `{"addr":"`+addr+`"}`, alice.tokenHdr()); code != http.StatusOK {
		t.Fatalf("add: %d %s", code, body)
	}
	waitFor(t, "alice and bob connected", func() bool {
		n := alice.app.node()
		return n != nil && n.PeerHas("bob", node.CapChat) && bob.app.node().Connected("alice")
	})
	return alice, bob
}

func decodeAs[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

func TestUIChatEndpoints(t *testing.T) {
	alice, bob := connectedPair(t)
	hdr := alice.tokenHdr()
	if code, body := alice.do(t, http.MethodGet, "/ui/api/chats", "", nil); code != http.StatusForbidden {
		t.Fatalf("chats without the token: %d %s", code, body)
	}
	alice.app.events.mu.Lock()
	before := alice.app.events.revision
	alice.app.events.mu.Unlock()
	code, body := alice.do(t, http.MethodPost, "/ui/api/chats", `{"participants":["bob"]}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	chat := decodeAs[node.ChatInfo](t, body)
	if len(chat.Participants) != 2 || chat.Closed {
		t.Fatalf("created %+v", chat)
	}
	alice.app.events.mu.Lock()
	published := alice.app.events.topics["chats"] > before
	alice.app.events.mu.Unlock()
	if !published {
		t.Fatal("creating a chat published no chats event")
	}
	code, body = alice.do(t, http.MethodPost, "/ui/api/send", `{"chat_id":"`+chat.ID+`","body":"hello bob","ask":["bob"]}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	sent := decodeAs[node.Message](t, body)
	waitFor(t, "bob has the message", func() bool {
		code, body := bob.do(t, http.MethodGet, "/ui/api/chats/"+chat.ID+"/messages", "", bob.tokenHdr())
		return code == http.StatusOK && strings.Contains(body, sent.ID)
	})
	code, body = alice.do(t, http.MethodGet, "/ui/api/chats/"+chat.ID+"/messages?limit=10", "", hdr)
	msgs := decodeAs[[]node.ChatMessage](t, body)
	if code != http.StatusOK || len(msgs) != 2 || msgs[0].Kind != node.KindChatOpen || msgs[1].ID != sent.ID || msgs[1].Direction != "out" {
		t.Fatalf("messages: %d %s", code, body)
	}
	if code, body = alice.do(t, http.MethodGet, "/ui/api/chats/"+chat.ID, "", hdr); code != http.StatusOK ||
		decodeAs[node.ChatInfo](t, body).Count != 1 {
		t.Fatalf("chat: %d %s", code, body)
	}
	if code, body = alice.do(t, http.MethodPost, "/ui/api/chats/"+chat.ID+"/close", "", hdr); code != http.StatusOK ||
		!decodeAs[node.ChatInfo](t, body).Closed {
		t.Fatalf("close: %d %s", code, body)
	}
	code, body = alice.do(t, http.MethodPost, "/ui/api/send", `{"chat_id":"`+chat.ID+`","body":"more"}`, hdr)
	if code != http.StatusBadRequest || errorText(t, body) != uiStrings["error.chat_closed"] {
		t.Fatalf("send to a closed chat: %d %s", code, body)
	}
	if code, body = alice.do(t, http.MethodGet, "/ui/api/chats?archive=1", "", hdr); code != http.StatusOK ||
		len(decodeAs[[]node.ChatInfo](t, body)) != 1 {
		t.Fatalf("archive: %d %s", code, body)
	}
	if code, body = alice.do(t, http.MethodPost, "/ui/api/chats/"+chat.ID+"/archive", `{"archived":false}`, hdr); code != http.StatusOK ||
		decodeAs[node.ChatInfo](t, body).Archived {
		t.Fatalf("unarchive: %d %s", code, body)
	}
	code, body = alice.do(t, http.MethodGet, "/ui/api/chats/"+strings.Repeat("0", 32), "", hdr)
	if code != http.StatusNotFound || errorText(t, body) != uiStrings["error.unknown_chat"] {
		t.Fatalf("unknown chat: %d %s", code, body)
	}
	code, body = alice.do(t, http.MethodPost, "/ui/api/chats", `{"participants":`, hdr)
	if code != http.StatusBadRequest || errorText(t, body) != uiStrings["error.bad_request"] {
		t.Fatalf("bad body: %d %s", code, body)
	}
	code, body = alice.do(t, http.MethodPost, "/ui/api/chats", `{"participants":["carol"]}`, hdr)
	if code != http.StatusBadRequest || errorText(t, body) != uiStrings["error.unknown_peer"] {
		t.Fatalf("unknown member: %d %s", code, body)
	}
	// Legacy history is listed as a virtual chat.
	if code, body = alice.do(t, http.MethodPost, "/ui/api/send", `{"to":"bob","body":"old style"}`, hdr); code != http.StatusOK {
		t.Fatalf("legacy send: %d %s", code, body)
	}
	_, body = alice.do(t, http.MethodGet, "/ui/api/chats", "", hdr)
	chats := decodeAs[[]node.ChatInfo](t, body)
	legacy := 0
	for _, c := range chats {
		if c.Legacy {
			legacy++
		}
	}
	if legacy != 1 {
		t.Fatalf("list: %s", body)
	}
	// The composer of a legacy chat continues it in a real chat.
	var legacyID string
	for _, c := range chats {
		if c.Legacy {
			legacyID = c.ID
		}
	}
	code, body = alice.do(t, http.MethodPost, "/ui/api/send", `{"chat_id":"`+legacyID+`","body":"go on","ask":["bob"]}`, hdr)
	if next := decodeAs[node.Message](t, body); code != http.StatusOK || next.ChatID == "" || next.ChatID == chat.ID {
		t.Fatalf("send to a legacy chat: %d %s", code, body)
	}
}
