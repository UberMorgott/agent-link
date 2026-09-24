package app

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// freeAddr is a loopback address whose port was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// pairApps starts alice and bob, both in the legacy network and in project
// p; bob dials alice. Alice keeps p's folder, bob binds one of his own.
func pairApps(t *testing.T, p settings.ProjectBinding, more ...settings.ProjectBinding) (alice, bob *App) {
	t.Helper()
	const code = "K7Q2-MXPA-4RTB"
	addr := freeAddr(t)
	alice = startApp(t, settings.Settings{Code: code, Listen: addr, Bindings: append([]settings.ProjectBinding{p}, more...)})
	pb := p
	pb.Dir, pb.Peers = t.TempDir(), []string{addr}
	bob = startApp(t, settings.Settings{Node: "bob", Code: code, Peers: []config.Peer{{Addr: addr}}, Bindings: []settings.ProjectBinding{pb}})
	eventuallyLong(t, "alice and bob connected", func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return alice.n.Connected("bob") && alice.projects[p.ID].n.Connected("bob")
	})
	return alice, bob
}

// call sends one control API request to srv.
func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The control API routes by project, chat, message and folder (§6.1).
func TestControlAPIRoutes(t *testing.T) {
	p, noDir := newBinding(t, t.TempDir()), newBinding(t, "")
	alice, bob := pairApps(t, p, noDir)
	srv := httptest.NewServer(alice.Handler())
	defer srv.Close()
	alice.mu.Lock()
	legacyN, projN := alice.n, alice.projects[p.ID].n
	alice.mu.Unlock()
	bob.mu.Lock()
	bobProj := bob.projects[p.ID].n
	bob.mu.Unlock()

	legacyChat, err := legacyN.CreateChat([]string{"bob"}, "")
	if err != nil {
		t.Fatal(err)
	}
	projChat, err := projN.NewProjectChat([]string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("ab", 16)

	// Chat lists: every context without a selector, each naming its project.
	code, body := call(t, srv, http.MethodGet, "/chats", "")
	var all []ChatInfoView
	if err := json.Unmarshal([]byte(body), &all); code != http.StatusOK || err != nil {
		t.Fatalf("GET /chats: %d %s", code, body)
	}
	projects := map[string]string{}
	for _, c := range all {
		projects[c.ID] = c.Project
	}
	if projects[legacyChat.ID] != LegacyProjectID || projects[projChat.ID] != p.ID {
		t.Fatalf("chat projects %v", projects)
	}
	if code, body := call(t, srv, http.MethodGet, "/chats?project="+p.ID, ""); code != http.StatusOK ||
		!strings.Contains(body, projChat.ID) || strings.Contains(body, legacyChat.ID) {
		t.Fatalf("GET /chats?project: %d %s", code, body)
	}
	for path, want := range map[string]int{
		"/chats?project=" + unknown:                          http.StatusNotFound,
		"/chats/" + projChat.ID:                              http.StatusOK,
		"/chats/" + projChat.ID + "/messages":                http.StatusOK,
		"/chats/" + projChat.ID + "?project=legacy":          http.StatusConflict,
		"/chats/" + legacyChat.ID + "?project=" + p.ID:       http.StatusConflict,
		"/chats/" + unknown:                                  http.StatusNotFound,
		"/members?project=" + p.ID:                           http.StatusOK,
		"/inbox":                                             http.StatusOK,
		"/wait?timeout=10ms&chat=" + projChat.ID:             http.StatusNoContent,
		"/wait?timeout=10ms&project=" + noDir.ID:             http.StatusConflict,
		"/unread?project=" + noDir.ID:                        http.StatusConflict,
		"/unread?project=" + p.ID + "&folder=" + t.TempDir(): http.StatusConflict,
		"/unread?folder=" + p.Dir:                            http.StatusOK,
	} {
		if code, body := call(t, srv, http.MethodGet, path, ""); code != want {
			t.Errorf("GET %s: %d %s, want %d", path, code, body, want)
		}
	}

	// Send: chat owner, explicit project, folder, legacy.
	send := func(req map[string]any) (int, node.Message) {
		t.Helper()
		code, body := call(t, srv, http.MethodPost, "/send", jsonOf(t, req))
		var m node.Message
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatal(err)
			}
		}
		return code, m
	}
	if code, m := send(map[string]any{"chat_id": projChat.ID, "body": "hi"}); code != http.StatusOK || m.ChatID != projChat.ID {
		t.Fatalf("send into the project chat: %d %+v", code, m)
	}
	if code, _ := send(map[string]any{"chat_id": projChat.ID, "body": "hi", "project": LegacyProjectID}); code != http.StatusConflict {
		t.Fatalf("send with a foreign project: %d", code)
	}
	if code, _ := send(map[string]any{"to": "bob", "body": "hi", "project": unknown}); code != http.StatusNotFound {
		t.Fatalf("send to an unknown project: %d", code)
	}
	if code, m := send(map[string]any{"to": "bob", "body": "hi", "folder": p.Dir}); code != http.StatusOK || !projN.OwnsChat(m.ChatID) {
		t.Fatalf("send from the project folder: %d %+v", code, m)
	}
	if code, m := send(map[string]any{"to": "bob", "body": "hi", "folder": t.TempDir()}); code != http.StatusOK || !legacyN.OwnsChat(m.ChatID) {
		t.Fatalf("send from another folder: %d %+v", code, m)
	}
	// A reply goes to the context of the message it answers.
	eventuallyLong(t, "bob has the project chat", func() bool { return bobProj.OwnsChat(projChat.ID) })
	fromBob, err := bobProj.SendRequest(node.SendRequest{ChatID: projChat.ID, Body: "from bob"})
	if err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "alice got bob's message", func() bool { return projN.OwnsMessage(fromBob.ID) })
	if code, m := send(map[string]any{"reply_to": fromBob.ID, "body": "thanks"}); code != http.StatusOK || m.ChatID != projChat.ID {
		t.Fatalf("reply: %d %+v", code, m)
	}
	// Ack without a chat: each id to its owner; an unknown one is not found.
	code, body = call(t, srv, http.MethodPost, "/ack", jsonOf(t, node.AckRequest{IDs: []string{fromBob.ID, unknown}}))
	var acks []node.AckResult
	if err := json.Unmarshal([]byte(body), &acks); code != http.StatusOK || err != nil || len(acks) != 2 || !acks[0].Found || acks[1].Found {
		t.Fatalf("ack: %d %s", code, body)
	}

	// Sessions: by folder, remembered for the end, listed with their project.
	sess := func(req map[string]any) int {
		t.Helper()
		code, _ := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, req))
		return code
	}
	if code := sess(map[string]any{"session_id": "s1", "provider": "claude", "folder": p.Dir}); code != http.StatusOK {
		t.Fatalf("register in the project folder: %d", code)
	}
	if len(projN.Sessions()) != 1 || len(legacyN.Sessions()) != 0 {
		t.Fatal("session registered in the wrong context")
	}
	if code := sess(map[string]any{"session_id": "s2", "provider": "claude", "folder": p.Dir, "project": noDir.ID}); code != http.StatusConflict {
		t.Fatalf("register in a project without a folder: %d", code)
	}
	if code := sess(map[string]any{"session_id": "s3", "provider": "claude", "folder": t.TempDir(), "project": p.ID}); code != http.StatusConflict {
		t.Fatalf("register outside the project folder: %d", code)
	}
	code, body = call(t, srv, http.MethodGet, "/sessions", "")
	if code != http.StatusOK || !strings.Contains(body, `"project":"`+p.ID+`"`) {
		t.Fatalf("GET /sessions: %d %s", code, body)
	}
	if code, _ := call(t, srv, http.MethodDelete, "/sessions/s1", ""); code != http.StatusNoContent {
		t.Fatalf("end session: %d", code)
	}
	if code, _ := call(t, srv, http.MethodDelete, "/sessions/s1", ""); code != http.StatusNotFound {
		t.Fatalf("end an unknown session: %d", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/chats", ""); code != http.StatusOK {
		t.Fatal("browsers are refused, the CLI is not")
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/chats", nil)
	req.Header.Set("Origin", srv.URL)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser request: %v %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
}

// Without the legacy network a folder outside every project reaches no
// context.
func TestControlAPINoContext(t *testing.T) {
	p := newBinding(t, t.TempDir())
	a := startApp(t, settings.Settings{Bindings: []settings.ProjectBinding{p}})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	if code, body := call(t, srv, http.MethodPost, "/send", `{"to":"bob","body":"hi","folder":`+jsonOf(t, t.TempDir())+`}`); code != http.StatusBadRequest ||
		!strings.Contains(body, "folder is not in a project") {
		t.Fatalf("send outside every project: %d %s", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/unread?folder="+p.Dir, ""); code != http.StatusOK {
		t.Fatalf("unread in the project folder: %d %s", code, body)
	}
}

func eventuallyLong(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
