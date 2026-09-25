package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
)

// mcpClient connects an in-memory MCP client to the agentlink MCP server on api.
func mcpClient(t *testing.T, api string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newMCPServer(config.Config{API: api}).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs
}

// callTool calls a tool and returns its text and error flag.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: content %+v", name, res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s: content %T", name, res.Content[0])
	}
	return tc.Text, res.IsError
}

// linesToArray turns the CLI's JSON lines into one normalized JSON array.
func linesToArray(t *testing.T, out string) string {
	t.Helper()
	items := []json.RawMessage{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" {
			items = append(items, json.RawMessage(line))
		}
	}
	return normJSON(t, toJSON(t, items))
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func normJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not JSON: %q", s)
	}
	return toJSON(t, v)
}

// cleanAgentEnv keeps the test's own agent session and job out of the commands.
func cleanAgentEnv(t *testing.T) {
	for _, k := range []string{envClaudeSession, envCodexThread, envCodexSession, envJobID, envChatID, envProjectID, envAPI} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
}

// Every tool sends the requests of its CLI command and answers with the same
// JSON (a list as one array); projects is GET /projects.
func TestMCPToolsMatchCLIRequests(t *testing.T) {
	f := newFakeAPI(t)
	cleanAgentEnv(t)
	cs := mcpClient(t, f.api)
	for _, c := range []struct {
		cli  []string
		tool string
		args map[string]any
	}{
		{[]string{"projects"}, "projects", nil},
		{[]string{"members", "--project", "P1"}, "members", map[string]any{"project": "P1"}},
		{[]string{"members"}, "members", nil},
		{[]string{"chat", "list", "--archive", "--legacy", "--project", "P1"}, "chats", map[string]any{"archive": true, "legacy": true, "project": "P1"}},
		{[]string{"chat", "history", "--chat", "c1", "--before", "9", "--after", "3"}, "history", map[string]any{"chat": "c1", "before_seq": 9, "after_seq": 3}},
		{[]string{"chat", "history", "--chat", "c1"}, "history", map[string]any{"chat": "c1"}},
		{[]string{"chat", "ack", "--chat", "c1", "--ids", "m1,m2", "--project", "P1"}, "ack", map[string]any{"chat": "c1", "ids": []string{"m1", "m2"}, "project": "P1"}},
	} {
		f.reqs = nil
		cliOut := f.run(c.cli...)
		cliReqs := f.reqs
		f.reqs = nil
		text, isErr := callTool(t, cs, c.tool, c.args)
		if isErr {
			t.Fatalf("%s: error %s", c.tool, text)
		}
		if !slices.Equal(cliReqs, f.reqs) {
			t.Fatalf("%s: requests %q, CLI %q", c.tool, f.reqs, cliReqs)
		}
		if got, want := normJSON(t, text), linesToArray(t, cliOut); got != want {
			t.Fatalf("%s: %s, CLI %s", c.tool, got, want)
		}
	}
	// unread: the CLI's lines are the page's messages, its last line next and total.
	f.reqs = nil
	cliOut := f.run("chat", "unread", "--limit", "1", "--after", "0-x", "--folder", ".")
	cliReqs := f.reqs
	f.reqs = nil
	text, isErr := callTool(t, cs, "unread", map[string]any{"limit": 1, "after": "0-x", "folder": "."})
	if isErr || !slices.Equal(cliReqs, f.reqs) {
		t.Fatalf("unread: %s, requests %q, CLI %q", text, f.reqs, cliReqs)
	}
	var page node.UnreadPage
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatal(err)
	}
	var lines []any
	for _, m := range page.Messages {
		lines = append(lines, m)
	}
	lines = append(lines, map[string]any{"next": page.Next, "total": page.Total})
	if got, want := normJSON(t, toJSON(t, lines)), linesToArray(t, cliOut); got != want || page.Next == "" {
		t.Fatalf("unread %s, CLI %s", got, want)
	}
	// send: the same request body as send --chat.
	f.reqs = nil
	f.run("send", "--chat", "c1", "--body", "hi", "--ask", "b", "--reply-to", "r1")
	cliSend := f.send
	text, isErr = callTool(t, cs, "send", map[string]any{"chat": "c1", "body": "hi", "ask": []string{"b"}, "reply_to": "r1"})
	if isErr || normJSON(t, text) != `{"chat":"","id":"m1"}` || toJSON(t, f.send) != toJSON(t, cliSend) {
		t.Fatalf("send: %s, request %+v, CLI %+v", text, f.send, cliSend)
	}
}

// send takes exactly one of chat, to and new_chat_with, and a body; nothing
// reaches the API otherwise.
func TestMCPSendRoutingModes(t *testing.T) {
	f := newFakeAPI(t)
	cleanAgentEnv(t)
	cs := mcpClient(t, f.api)
	for _, args := range []map[string]any{
		{"body": "hi"},
		{"body": "hi", "chat": "c1", "to": "b"},
		{"body": "hi", "chat": "c1", "new_chat_with": []string{"b"}},
		{"body": "hi", "to": "b", "new_chat_with": []string{"b"}},
		{"body": "hi", "chat": "c1", "to": "b", "new_chat_with": []string{"b"}},
	} {
		text, isErr := callTool(t, cs, "send", args)
		if !isErr || !strings.Contains(text, "exactly one of chat, to, new_chat_with") {
			t.Fatalf("send %v: %q, error %v", args, text, isErr)
		}
	}
	if text, isErr := callTool(t, cs, "send", map[string]any{"chat": "c1", "body": ""}); !isErr || !strings.Contains(text, "body is required") {
		t.Fatalf("send without body: %q", text)
	}
	if len(f.reqs) != 0 {
		t.Fatalf("requests %q", f.reqs)
	}
	for _, c := range []struct {
		args map[string]any
		reqs []string
		send node.SendRequest
	}{
		{map[string]any{"body": "hi", "to": "b"}, []string{"POST /send"}, node.SendRequest{To: "b"}},
		{map[string]any{"body": "hi", "new_chat_with": []string{"b", "c"}}, []string{"POST /chats?cwd=", "POST /send"}, node.SendRequest{ChatID: "c1"}},
	} {
		f.reqs = nil
		if text, isErr := callTool(t, cs, "send", c.args); isErr {
			t.Fatalf("send %v: %s", c.args, text)
		}
		if len(f.reqs) != len(c.reqs) || f.send.To != c.send.To || f.send.ChatID != c.send.ChatID || f.send.Body != "hi" {
			t.Fatalf("send %v: requests %q, request %+v", c.args, f.reqs, f.send)
		}
		for i, r := range c.reqs {
			if !strings.HasPrefix(f.reqs[i], r) {
				t.Fatalf("send %v: requests %q", c.args, f.reqs)
			}
		}
	}
}

// The session of ack and send defaults to the agent's own: Claude's, Codex's
// thread, else Codex's session id.
func TestMCPSessionFromEnv(t *testing.T) {
	f := newFakeAPI(t)
	cleanAgentEnv(t)
	cs := mcpClient(t, f.api)
	t.Setenv(envCodexSession, "codex-s")
	callTool(t, cs, "send", map[string]any{"to": "b", "body": "hi"})
	if f.send.SessionID != "codex-s" {
		t.Fatalf("send session %q", f.send.SessionID)
	}
	t.Setenv(envClaudeSession, "claude-s")
	callTool(t, cs, "send", map[string]any{"to": "b", "body": "hi"})
	if f.send.SessionID != "claude-s" {
		t.Fatalf("send session %q", f.send.SessionID)
	}
}

// Against two real nodes the tools answer as the CLI does, unread marks
// nothing read, ack does, and an API error comes back verbatim.
func TestMCPParityRealNode(t *testing.T) {
	cleanAgentEnv(t)
	lnA, lnB := listenMCP(t), listenMCP(t)
	a := startMCPNode(t, "a", lnA, "b", lnB)
	b := startMCPNode(t, "b", lnB, "a", lnA)
	api := strings.TrimPrefix(a.api.URL, "http://")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"node":"a","listen":"127.0.0.1:0","api":"`+api+`","data_dir":"`+
		filepath.ToSlash(t.TempDir())+`","secret_env":"X"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := run(append(args, "--config", cfgPath), &out, &errw); code != 0 {
			t.Fatalf("%v: code %d, stderr %s", args, code, errw.String())
		}
		return out.String()
	}
	cs := mcpClient(t, api)

	var chat node.ChatInfo
	var err error
	waitFor(t, "chat", func() bool { chat, err = b.n.CreateChat([]string{"a"}, ""); return err == nil })
	var sent []string
	for _, body := range []string{"one", "two", "three"} {
		m, err := b.n.SendChat(node.ChatSend{ChatID: chat.ID, Body: body, Ask: []string{"a"}})
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, m.ID)
	}
	unreadText := func() string {
		text, isErr := callTool(t, cs, "unread", nil)
		if isErr {
			t.Fatal(text)
		}
		return text
	}
	waitFor(t, "unread on a", func() bool { return strings.Contains(unreadText(), sent[2]) })

	parity := func(tool string, args map[string]any, cliArgs ...string) {
		t.Helper()
		var got, want string
		for range 5 { // a member's seen time may move between the two calls
			text, isErr := callTool(t, cs, tool, args)
			if isErr {
				t.Fatalf("%s: %s", tool, text)
			}
			if got, want = normJSON(t, text), linesToArray(t, cli(cliArgs...)); got == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s: %s, CLI %s", tool, got, want)
	}
	parity("members", nil, "members")
	parity("chats", nil, "chat", "list")
	parity("chats", map[string]any{"archive": true}, "chat", "list", "--archive")
	parity("history", map[string]any{"chat": chat.ID}, "chat", "history", "--chat", chat.ID)
	parity("history", map[string]any{"chat": chat.ID, "after_seq": 1, "limit": 1}, "chat", "history", "--chat", chat.ID, "--after", "1", "--limit", "1")

	// unread: the CLI's lines are the messages; reading marks nothing read.
	before := unreadText()
	var page node.UnreadPage
	if err := json.Unmarshal([]byte(before), &page); err != nil || len(page.Messages) != 3 || page.Total != 3 {
		t.Fatalf("unread %s", before)
	}
	if got, want := normJSON(t, toJSON(t, page.Messages)), linesToArray(t, cli("chat", "unread")); got != want {
		t.Fatalf("unread %s, CLI %s", got, want)
	}
	if after := unreadText(); after != before {
		t.Fatalf("unread changed: %s, then %s", before, after)
	}

	// ack: CLI and tool each mark one message read, with the same answer.
	cliAck := linesToArray(t, cli("chat", "ack", "--chat", chat.ID, "--ids", sent[0]))
	text, isErr := callTool(t, cs, "ack", map[string]any{"chat": chat.ID, "ids": []string{sent[1]}})
	if isErr || strings.ReplaceAll(normJSON(t, text), sent[1], "ID") != strings.ReplaceAll(cliAck, sent[0], "ID") {
		t.Fatalf("ack %s, CLI %s", text, cliAck)
	}
	if err := json.Unmarshal([]byte(unreadText()), &page); err != nil || len(page.Messages) != 1 || page.Messages[0].ID != sent[2] {
		t.Fatalf("unread after ack %+v", page)
	}

	// send into the chat: {id, chat}, and the message is in its history.
	text, isErr = callTool(t, cs, "send", map[string]any{"chat": chat.ID, "body": "reply from mcp"})
	var res map[string]string
	if err := json.Unmarshal([]byte(text), &res); isErr || err != nil || res["chat"] != chat.ID || res["id"] == "" {
		t.Fatalf("send %s", text)
	}
	if h := cli("chat", "history", "--chat", chat.ID); !strings.Contains(h, res["id"]) || !strings.Contains(h, "reply from mcp") {
		t.Fatalf("history after send %s", h)
	}

	// An API error is the API's message, verbatim.
	var errw bytes.Buffer
	if code := run([]string{"chat", "history", "--chat", "nope", "--config", cfgPath}, &bytes.Buffer{}, &errw); code != 1 {
		t.Fatalf("CLI history of an unknown chat: code %d", code)
	}
	text, isErr = callTool(t, cs, "history", map[string]any{"chat": "nope"})
	if !isErr || text == "" || !strings.HasSuffix(strings.TrimSpace(errw.String()), ": "+text) {
		t.Fatalf("history error %q, CLI %q", text, errw.String())
	}
}

type mcpNode struct {
	n   *node.Node
	api *httptest.Server
}

func listenMCP(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startMCPNode runs a real node with its control API on a test server.
func startMCPNode(t *testing.T, name string, ln net.Listener, peer string, peerLn net.Listener) *mcpNode {
	t.Helper()
	cfg := config.Config{
		Node: name, Listen: ln.Addr().String(), API: "127.0.0.1:0", DataDir: t.TempDir(), SecretEnv: "UNUSED",
		Peers: []config.Peer{{Name: peer, Addr: peerLn.Addr().String()}},
	}
	n, err := node.New(cfg, []byte("test-secret-0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &mcpNode{n: n, api: httptest.NewServer(n.APIHandler())}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { n.Run(ctx, ln) })
	t.Cleanup(func() { cancel(); wg.Wait(); e.api.Close() })
	return e
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
