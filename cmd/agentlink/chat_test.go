package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// fakeAPI records the requests of one CLI command and answers them.
type fakeAPI struct {
	t    *testing.T
	cfg  string
	api  string // host:port of the fake API
	reqs []string
	send node.SendRequest
}

func newFakeAPI(t *testing.T) *fakeAPI {
	// Tests may run inside an agent session: never pick up (or touch) its id.
	t.Setenv(envClaudeSession, "")
	t.Setenv(envCodexThread, "")
	f := &fakeAPI{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/send":
			f.send = node.SendRequest{}
			_ = json.NewDecoder(r.Body).Decode(&f.send)
			_ = json.NewEncoder(w).Encode(node.Message{ID: "m1"})
		case r.URL.Path == "/chats" && r.Method == http.MethodPost, strings.HasSuffix(r.URL.Path, "/close"), strings.HasSuffix(r.URL.Path, "/archive"):
			_ = json.NewEncoder(w).Encode(node.ChatInfo{ID: "c1"})
		case strings.HasSuffix(r.URL.Path, "/ack"):
			_ = json.NewEncoder(w).Encode([]node.AckResult{{ID: "m1", Found: true}, {ID: "m2", Found: true}})
		case r.URL.Path == "/wait" && r.URL.Query().Get("timeout") == "2s":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/unread":
			_ = json.NewEncoder(w).Encode(node.UnreadPage{Messages: []node.UnreadMessage{{Cursor: "1-m1"}}, Total: 3, Next: "1-m1"})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_ = json.NewEncoder(w).Encode([]node.ChatMessage{{Seq: 1}, {Seq: 2}})
		default:
			_ = json.NewEncoder(w).Encode([]node.ChatInfo{})
		}
	}))
	t.Cleanup(srv.Close)
	f.api = strings.TrimPrefix(srv.URL, "http://")
	f.cfg = filepath.Join(t.TempDir(), "config.json")
	cfg := `{"node":"a","listen":"127.0.0.1:0","api":"` + f.api + `","data_dir":"` +
		filepath.ToSlash(t.TempDir()) + `","secret_env":"X"}`
	if err := os.WriteFile(f.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeAPI) run(args ...string) string {
	f.t.Helper()
	var out, errw bytes.Buffer
	if code := run(append(args, "--config", f.cfg), &out, &errw); code != 0 {
		f.t.Fatalf("%v: code %d, stderr %s", args, code, errw.String())
	}
	return out.String()
}

func TestChatCommands(t *testing.T) {
	f := newFakeAPI(t)
	if out := f.run("chat", "new", "--with", "b,c", "--area", "dev"); strings.TrimSpace(out) != "c1" {
		t.Fatalf("chat new printed %q", out)
	}
	f.run("chat", "list", "--archive")
	if out := f.run("chat", "history", "--chat", "c1", "--limit", "5", "--after", "3"); strings.Count(out, "\n") != 2 {
		t.Fatalf("chat history printed %q", out)
	}
	if out := f.run("chat", "ack", "--chat", "c1", "--ids", "m1, m2", "--session", "s1"); strings.Count(out, "\n") != 2 {
		t.Fatalf("chat ack printed %q", out)
	}
	f.run("chat", "ack", "--ids", "m3")
	if out := f.run("chat", "unread", "--limit", "1", "--after", "0-x"); strings.Count(out, "\n") != 2 || !strings.Contains(out, `{"next":"1-m1","total":3}`) {
		t.Fatalf("chat unread printed %q", out)
	}
	f.run("wait", "--chat", "c1", "--timeout", "1")
	want := []string{
		"POST /chats?" + url.Values{"cwd": {cwd(t)}}.Encode(),
		"GET /chats?archive=1",
		"GET /chats/c1/messages?after=3&limit=5",
		"POST /chats/c1/ack",
		"POST /ack",
		"GET /unread?" + url.Values{"after": {"0-x"}, "cwd": {cwd(t)}, "limit": {"1"}}.Encode(),
		"GET /wait?" + url.Values{"chat": {"c1"}, "folder": {cwd(t)}, "timeout": {"1s"}}.Encode(),
	}
	if strings.Join(f.reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.reqs, "\n"), strings.Join(want, "\n"))
	}
}

// Agents never close chats: only people do, in the app.
func TestCloseRefused(t *testing.T) {
	f := newFakeAPI(t)
	var out, errw bytes.Buffer
	if code := run([]string{"close", "--chat", "c1", "--config", f.cfg}, &out, &errw); code == 0 || !strings.Contains(errw.String(), "a person closes") {
		t.Fatalf("close in a job: code %d, stderr %q", code, errw.String())
	}
	if len(f.reqs) != 0 {
		t.Fatalf("requests %q", f.reqs)
	}
}

// Without --config a client command uses $AGENTLINK_API, else the desktop
// app's settings file.
func TestClientWithoutConfig(t *testing.T) {
	f := newFakeAPI(t)
	runNoConfig := func(args ...string) {
		t.Helper()
		var out, errw bytes.Buffer
		if code := run(args, &out, &errw); code != 0 {
			t.Fatalf("%v: code %d, stderr %s", args, code, errw.String())
		}
	}
	t.Setenv(envAPI, f.api)
	t.Setenv(envChatID, "c1")
	runNoConfig("chat", "history")
	runNoConfig("send", "--body", "hi")
	if f.send.ChatID != "c1" {
		t.Fatalf("send via $%s = %+v", envAPI, f.send)
	}

	t.Setenv(envAPI, "")
	dir := t.TempDir()
	t.Setenv("AppData", dir)         // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", dir) // and on Unix
	t.Setenv("HOME", dir)            // and on macOS (Library/Application Support)
	p, err := settings.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"api":"`+f.api+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runNoConfig("members")
	if last := f.reqs[len(f.reqs)-1]; last != "GET /members?"+(url.Values{"cwd": {cwd(t)}}).Encode() {
		t.Fatalf("members via settings: last request %q", last)
	}
}

// Inside a job the agent's send goes to the job's chat and continues its chain;
// every send names the job, so its own reply is not taken for someone else's.
func TestSendInsideJobUsesChatContext(t *testing.T) {
	f := newFakeAPI(t)
	t.Setenv(envChatID, "c1")
	t.Setenv(envJobID, "j1")
	f.run("send", "--body", "next", "--ask", "c")
	if s := f.send; s.ChatID != "c1" || s.Parent != "j1" || len(s.Ask) != 1 || s.Ask[0] != "c" || s.To != "" {
		t.Fatalf("send in a job = %+v", s)
	}
	f.run("send", "--to", "b", "--body", "plain")
	if s := f.send; s.ChatID != "" || s.Parent != "j1" || s.To != "b" {
		t.Fatalf("send --to in a job = %+v", s)
	}
	f.run("send", "--chat", "other", "--body", "elsewhere")
	if s := f.send; s.ChatID != "other" || s.Parent != "j1" {
		t.Fatalf("send to another chat = %+v", s)
	}
}

// A send inside an agent session names the session, so the reply goes back
// to it and to no other session of the folder; --session overrides it, and a
// worker's job (no session) names none.
func TestSendNamesItsSession(t *testing.T) {
	f := newFakeAPI(t)
	t.Setenv(envJobID, "")
	dir := t.TempDir() // the hook state send notes the chat in
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	f.run("send", "--to", "b", "--body", "hi")
	if f.send.SessionID != "" {
		t.Fatalf("outside a session: %+v", f.send)
	}
	t.Setenv(envClaudeSession, "sess-claude")
	f.run("send", "--to", "b", "--body", "hi")
	if f.send.SessionID != "sess-claude" {
		t.Fatalf("claude session: %+v", f.send)
	}
	f.run("send", "--to", "b", "--body", "hi", "--session", "explicit")
	if f.send.SessionID != "explicit" {
		t.Fatalf("--session: %+v", f.send)
	}
	t.Setenv(envClaudeSession, "")
	t.Setenv(envCodexThread, "thread-1")
	f.run("send", "--to", "b", "--body", "hi")
	if f.send.SessionID != "thread-1" {
		t.Fatalf("codex thread: %+v", f.send)
	}
	t.Setenv(envJobID, "j1")
	f.run("send", "--to", "b", "--body", "hi")
	if f.send.SessionID != "" {
		t.Fatalf("inside a job: %+v", f.send)
	}
}

func cwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// send, wait and chat name the project: --project, else the agent's own
// ($AGENTLINK_PROJECT_ID); send also names its folder, as wait does.
func TestProjectSelector(t *testing.T) {
	f := newFakeAPI(t)
	t.Setenv(envProjectID, "PENV")
	f.run("send", "--to", "b", "--body", "hi")
	if f.send.Folder != cwd(t) {
		t.Fatalf("send folder %q", f.send.Folder)
	}
	f.run("wait", "--timeout", "1")
	f.run("chat", "list", "--project", "legacy")
	f.run("chat", "history", "--chat", "c1")
	f.run("chat", "unread")
	f.run("chat", "ack", "--ids", "m1")
	f.run("chat", "new", "--with", "b")
	t.Setenv(envProjectID, "")
	f.run("chat", "list")
	want := []string{
		"POST /send?project=PENV",
		"GET /wait?" + url.Values{"folder": {cwd(t)}, "project": {"PENV"}, "timeout": {"1s"}}.Encode(),
		"GET /chats?project=legacy",
		"GET /chats/c1/messages?limit=50&project=PENV",
		"GET /unread?limit=50&project=PENV",
		"POST /ack?project=PENV",
		"POST /chats?project=PENV",
		"GET /chats",
	}
	if strings.Join(f.reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.reqs, "\n"), strings.Join(want, "\n"))
	}
}

// inbox and the member commands name the project too, else their folder; a
// wait that times out says so on stderr.
func TestMembersProjectAndWaitTimeout(t *testing.T) {
	f := newFakeAPI(t)
	f.run("members", "--project", "P1")
	f.run("inbox", "--limit", "5")
	f.run("add", "--addr", "10.0.0.1", "--project", "P1")
	f.run("remove", "--name", "b", "--project", "P1")
	want := []string{
		"GET /members?project=P1",
		"GET /inbox?" + url.Values{"cwd": {cwd(t)}, "limit": {"5"}}.Encode(),
		"POST /members?project=P1",
		"POST /members/remove?project=P1",
	}
	if strings.Join(f.reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.reqs, "\n"), strings.Join(want, "\n"))
	}
	var out, errw bytes.Buffer
	if code := run([]string{"wait", "--timeout", "2", "--config", f.cfg}, &out, &errw); code != exitTimeout ||
		out.Len() != 0 || !strings.Contains(errw.String(), "no message within 2s") {
		t.Fatalf("wait timeout: code %d, stdout %q, stderr %q", code, out.String(), errw.String())
	}
}
