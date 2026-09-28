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
	t        *testing.T
	cfg      string
	api      string // host:port of the fake API
	reqs     []string
	send     node.SendRequest
	reassign node.ReassignRequest
	pin      node.PinSessionRequest
	discuss  map[string]string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	// Tests may run inside an agent session: never pick up (or touch) its id.
	t.Setenv(envClaudeSession, "")
	t.Setenv(envCodexThread, "")
	t.Setenv(envCodexSession, "")
	f := &fakeAPI{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/discuss":
			f.discuss = map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&f.discuss)
			_ = json.NewEncoder(w).Encode(discussResult{Project: "p1", Chat: "c1", ID: "m1", Seat: "seat-codex"})
		case r.URL.Path == "/discuss/reply":
			// The caller acknowledges the reply itself: without the flag the
			// server reads it (legacy callers) and a lost reply is gone.
			if r.URL.Query().Get("client_ack") != "1" {
				t.Errorf("discuss reply wait without client_ack=1: %s", r.URL.RequestURI())
			}
			switch r.URL.Query().Get("timeout") {
			case "20ms":
				w.WriteHeader(http.StatusNoContent)
			case "21ms":
				_, _ = w.Write([]byte(`{"id":"m2",`)) // the reply is cut off: the caller never gets it
			default:
				_ = json.NewEncoder(w).Encode(node.Message{ID: "m2", ReplyTo: "m1", Body: "answer", Agent: &node.AgentRef{Seat: "seat-codex"}})
			}
		case r.URL.Path == "/send":
			f.send = node.SendRequest{}
			_ = json.NewDecoder(r.Body).Decode(&f.send)
			_ = json.NewEncoder(w).Encode(node.Message{ID: "m1"})
		case r.URL.Path == "/chats" && r.Method == http.MethodPost, strings.HasSuffix(r.URL.Path, "/close"), strings.HasSuffix(r.URL.Path, "/archive"):
			_ = json.NewEncoder(w).Encode(node.ChatInfo{ID: "c1"})
		case r.URL.Path == "/reassign":
			var req node.ReassignRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.reassign = req
			_ = json.NewEncoder(w).Encode(node.ChatMessage{Assigned: "session:" + req.SessionID, ID: req.ID})
		case r.URL.Path == "/session-pin":
			_ = json.NewDecoder(r.Body).Decode(&f.pin)
			_ = json.NewEncoder(w).Encode(node.PinnedSession{SessionID: f.pin.SessionID, Provider: node.ProviderCodex})
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

func TestDiscussCLIWaitsAndReadsPromptFile(t *testing.T) {
	f := newFakeAPI(t)
	dir := t.TempDir()
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("line one\nline two"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.run("discuss", "--with", "codex", "--prompt-file", promptFile, "--folder", dir)
	var got discussResult
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != "m1" || got.Reply == nil || got.Reply.Body != "answer" {
		t.Fatalf("discuss result: %q: %v", out, err)
	}
	// The reply is read only after the call decoded it, in its project.
	if f.discuss["body"] != "line one\nline two" || f.discuss["folder"] != dir || len(f.reqs) != 3 || f.reqs[2] != "POST /ack?project=p1" {
		t.Fatalf("discuss requests: %q, body %+v", f.reqs, f.discuss)
	}
	f.reqs = nil
	f.run("discuss", "--with", "codex", "--body", "later", "--async", "--topic", "design")
	if len(f.reqs) != 1 || f.reqs[0] != "POST /discuss" || f.discuss["topic"] != "design" {
		t.Fatalf("async requests: %q, body %+v", f.reqs, f.discuss)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"discuss", "--with", "codex", "--body", "timeout", "--timeout", "20ms", "--config", f.cfg}, &stdout, &stderr); code != exitTimeout {
		t.Fatalf("timeout exit: %d, out %q, err %q", code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || !got.TimedOut || got.ID != "m1" {
		t.Fatalf("timeout result: %q: %v", stdout.String(), err)
	}
	// A reply the call failed to read stays unread: the hooks deliver it.
	f.reqs = nil
	stdout.Reset()
	if code := run([]string{"discuss", "--with", "codex", "--body", "cut", "--timeout", "21ms", "--config", f.cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("cut reply succeeded: %q", stdout.String())
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "/ack") {
			t.Fatalf("a reply the caller never got was read: %q", f.reqs)
		}
	}
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
	if out := f.run("chat", "archive", "--project", "P1"); strings.TrimSpace(out) != "c1" {
		t.Fatalf("chat archive printed %q", out)
	}
	want := []string{
		"POST /chats?" + url.Values{"cwd": {cwd(t)}}.Encode(),
		"GET /chats?archive=1",
		"GET /chats/c1/messages?after=3&limit=5",
		"POST /chats/c1/ack",
		"POST /ack",
		"GET /unread?" + url.Values{"after": {"0-x"}, "cwd": {cwd(t)}, "limit": {"1"}}.Encode(),
		"GET /wait?" + url.Values{"chat": {"c1"}, "folder": {cwd(t)}, "timeout": {"1s"}}.Encode(),
		"POST /chats/archive?project=P1",
	}
	if strings.Join(f.reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.reqs, "\n"), strings.Join(want, "\n"))
	}
}

// chat reassign hands one unread message to a live session in its project.
func TestChatReassign(t *testing.T) {
	f := newFakeAPI(t)
	out := f.run("chat", "reassign", "--id", "m9", "--session", "s2", "--force", "--project", "P1")
	if !strings.Contains(out, `"assigned":"session:s2"`) || !strings.Contains(out, `"id":"m9"`) {
		t.Fatalf("chat reassign printed %q", out)
	}
	if len(f.reqs) != 1 || f.reqs[0] != "POST /reassign?project=P1" || !f.reassign.Force {
		t.Fatalf("requests %q", f.reqs)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"chat", "reassign", "--id", "m9", "--config", f.cfg}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--session") {
		t.Fatalf("reassign without a session: code %d, stderr %q", code, stderr.String())
	}
}

func TestSessionPin(t *testing.T) {
	f := newFakeAPI(t)
	out := f.run("session", "pin", "--session", "thread-chef", "--project", "P1")
	if !strings.Contains(out, `"session_id":"thread-chef"`) || f.pin.SessionID != "thread-chef" {
		t.Fatalf("session pin: %q, %+v", out, f.pin)
	}
	if len(f.reqs) != 1 || f.reqs[0] != "POST /session-pin?project=P1" {
		t.Fatalf("requests %q", f.reqs)
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
	// Hermetic: the test itself may run inside a Claude or Codex session.
	t.Setenv(envClaudeSession, "")
	t.Setenv(envCodexThread, "")
	t.Setenv(envCodexSession, "")
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
	t.Setenv(envCodexThread, "")
	t.Setenv(envCodexSession, "codex-sess")
	f.run("send", "--to", "b", "--body", "hi")
	if f.send.SessionID != "codex-sess" {
		t.Fatalf("codex session fallback: %+v", f.send)
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
