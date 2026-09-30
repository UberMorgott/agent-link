package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/agent-link/internal/config"
)

const (
	mcpCallHelperEnv = "AGENTLINK_TEST_MCPCALL_HELPER"
	mcpCallMarkerEnv = "AGENTLINK_TEST_MCPCALL_MARKER"
	mcpCallCrashEnv  = "AGENTLINK_TEST_MCPCALL_CRASH"
)

// TestMCPCallHelper makes this test binary act as the updated executable's
// `agentlink mcp-call` for TestMCPCallsRunInUpdatedExecutable, leaving a
// marker that it ran.
func TestMCPCallHelper(t *testing.T) {
	if os.Getenv(mcpCallHelperEnv) != "1" {
		t.Skip("run by TestMCPCallsRunInUpdatedExecutable")
	}
	i := slices.Index(os.Args, "mcp-call")
	_ = os.WriteFile(os.Getenv(mcpCallMarkerEnv), []byte(strings.Join(os.Args[i:], " ")), 0o600)
	if os.Getenv(mcpCallCrashEnv) == "1" {
		_, _ = io.WriteString(os.Stdout, mcpCallStarted+"\n")
		os.Exit(3) // started, then died without a result
	}
	os.Exit(run(os.Args[i:], os.Stdout, os.Stderr))
}

// Once an update replaced the executable, a running MCP server (a session
// started before the update) runs each call in the new executable, so the
// session gets the updated tools without a restart; an executable that cannot
// run it leaves the call to the server itself.
func TestMCPCallsRunInUpdatedExecutable(t *testing.T) {
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `[{"id":"p1","name":"Проект — один"}]`)
	}))
	t.Cleanup(api.Close)
	host := strings.TrimPrefix(api.URL, "http://")
	marker := filepath.Join(t.TempDir(), "ran")
	// The new process calls this server's API, not its own default.
	t.Setenv(envAPI, "127.0.0.1:1")
	t.Setenv(mcpCallHelperEnv, "1")
	t.Setenv(mcpCallMarkerEnv, marker)
	old := mcpCallArgs
	mcpCallArgs = func(tool string) []string {
		return []string{"-test.run=^TestMCPCallHelper$", "--", "mcp-call", "--tool", tool}
	}
	t.Cleanup(func() { mcpCallArgs = old })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var fresh atomic.Pointer[string]
	fresh.Store(new(string))
	tools := newMCPTools(config.Config{API: host}, func() string { return *fresh.Load() })
	st, ct := mcp.NewInMemoryTransports()
	ss, err := tools.s.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })

	here, isErr := callTool(t, cs, "projects", nil)
	if _, err := os.Stat(marker); isErr || err == nil {
		t.Fatalf("before the update: %s (delegated: %v)", here, err == nil)
	}
	fresh.Store(&self)
	there, isErr := callTool(t, cs, "projects", nil)
	ran, _ := os.ReadFile(filepath.Clean(marker))
	if isErr || there != here || string(ran) != "mcp-call --tool projects" {
		t.Fatalf("after the update: %q (here %q), ran %q", there, here, ran)
	}
	// A tool error of the new executable is the call's error.
	if text, isErr := callTool(t, cs, "history", map[string]any{"chat": ""}); !isErr || !strings.Contains(text, "chat is required") {
		t.Fatalf("delegated error: %q %v", text, isErr)
	}
	// A call that started and died is an error, never run a second time here.
	t.Setenv(mcpCallCrashEnv, "1")
	before := hits.Load()
	if text, isErr := callTool(t, cs, "projects", nil); !isErr || !strings.Contains(text, "did not finish projects") || hits.Load() != before {
		t.Fatalf("crashed call: %q %v, api hits %d -> %d", text, isErr, before, hits.Load())
	}
	t.Setenv(mcpCallCrashEnv, "")
	// An executable that cannot run the call: the server runs it itself.
	gone := filepath.Join(t.TempDir(), "gone.exe")
	fresh.Store(&gone)
	if text, isErr := callTool(t, cs, "projects", nil); isErr || text != here {
		t.Fatalf("fallback: %q %v", text, isErr)
	}
}

// mcp-call's result line says whether the call succeeded: a tool error is
// ok:false with the error, never ok:true.
func TestMCPCallResultLine(t *testing.T) {
	var out bytes.Buffer
	if err := runMCPCall(config.Config{API: "127.0.0.1:1"}, "history", strings.NewReader(`{"chat":""}`), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || lines[0] != mcpCallStarted || lines[1] != `{"ok":false,"error":"chat is required"}` {
		t.Fatalf("mcp-call output: %q", out.String())
	}
	out.Reset()
	if err := runMCPCall(config.Config{API: "127.0.0.1:1"}, "bogus", strings.NewReader(``), &out); err != nil || !strings.HasSuffix(strings.TrimSpace(out.String()), `{"ok":false,"error":"unknown tool bogus"}`) {
		t.Fatalf("unknown tool: %q %v", out.String(), err)
	}
}
