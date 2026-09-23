package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// fakeAPI records the requests of one CLI command and answers them.
type fakeAPI struct {
	t    *testing.T
	cfg  string
	reqs []string
	send node.SendRequest
}

func newFakeAPI(t *testing.T) *fakeAPI {
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
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_ = json.NewEncoder(w).Encode([]node.ChatMessage{{Seq: 1}, {Seq: 2}})
		default:
			_ = json.NewEncoder(w).Encode([]node.ChatInfo{})
		}
	}))
	t.Cleanup(srv.Close)
	f.cfg = filepath.Join(t.TempDir(), "config.json")
	cfg := `{"node":"a","listen":"127.0.0.1:0","api":"` + strings.TrimPrefix(srv.URL, "http://") + `","data_dir":"` +
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
	f.run("chat", "archive", "--chat", "c1", "--undo")
	f.run("close", "--chat", "c1")
	f.run("wait", "--chat", "c1", "--timeout", "1")
	want := []string{
		"POST /chats",
		"GET /chats?archive=1",
		"GET /chats/c1/messages?after=3&limit=5",
		"POST /chats/c1/archive",
		"POST /chats/c1/close",
		"GET /wait?chat=c1&timeout=1s",
	}
	if strings.Join(f.reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.reqs, "\n"), strings.Join(want, "\n"))
	}
}

// Inside a job the agent's send goes to the job's chat and continues its chain.
func TestSendInsideJobUsesChatContext(t *testing.T) {
	f := newFakeAPI(t)
	t.Setenv(envChatID, "c1")
	t.Setenv(envJobID, "j1")
	f.run("send", "--body", "next", "--ask", "c")
	if s := f.send; s.ChatID != "c1" || s.Parent != "j1" || len(s.Ask) != 1 || s.Ask[0] != "c" || s.To != "" {
		t.Fatalf("send in a job = %+v", s)
	}
	f.run("send", "--to", "b", "--body", "plain")
	if s := f.send; s.ChatID != "" || s.Parent != "" || s.To != "b" {
		t.Fatalf("send --to in a job = %+v", s)
	}
	f.run("send", "--chat", "other", "--body", "elsewhere")
	if s := f.send; s.ChatID != "other" || s.Parent != "j1" {
		t.Fatalf("send to another chat = %+v", s)
	}
}
