package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/agent-link/internal/config"
)

// failWriter fails every write: a caller whose output is gone.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// A discuss reply is read only once the caller handed it on: a reply the CLI
// could not print stays unread for the hooks.
func TestDiscussAcksOnlyDeliveredReply(t *testing.T) {
	f := newFakeAPI(t)
	var stderr bytes.Buffer
	if code := run([]string{"discuss", "--with", "codex", "--body", "q", "--config", f.cfg}, failWriter{}, &stderr); code != 1 {
		t.Fatalf("failed stdout: code %d", code)
	}
	for _, r := range f.reqs {
		if strings.Contains(r, "/ack") {
			t.Fatalf("an undelivered reply was read: %q", f.reqs)
		}
	}
	f.reqs = nil
	f.run("discuss", "--with", "codex", "--body", "q")
	if n := strings.Count(strings.Join(f.reqs, "\n"), "/ack"); n != 1 {
		t.Fatalf("delivered reply acked %d times: %q", n, f.reqs)
	}
}

// A cancelled MCP call ends its request to the node (a discuss long poll)
// and reads nothing.
func TestMCPCallCancellationReachesNode(t *testing.T) {
	cleanAgentEnv(t)
	var mu sync.Mutex
	var acked bool
	polling, cancelled := make(chan struct{}), make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discuss":
			_, _ = w.Write([]byte(`{"project":"p1","chat":"c1","id":"m1","seat":"s1"}`))
		case "/discuss/reply":
			close(polling)
			<-r.Context().Done()
			close(cancelled)
		default:
			mu.Lock()
			acked = acked || strings.HasSuffix(r.URL.Path, "/ack")
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(api.Close)
	cs := mcpClient(t, strings.TrimPrefix(api.URL, "http://"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "discuss", Arguments: map[string]any{"with": "codex", "body": "q"}})
		done <- err
	}()
	<-polling
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the node's request outlived the cancelled call")
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	if acked {
		t.Fatal("a cancelled call read the reply")
	}
}

// An ordinary command fails against a node that does not answer, instead of
// hanging.
func TestCommandTimesOutOnWedgedNode(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); api.Close() })
	old := apiTimeout
	apiTimeout = 50 * time.Millisecond
	t.Cleanup(func() { apiTimeout = old })
	start := time.Now()
	_, err := listProjects(t.Context(), config.Config{API: strings.TrimPrefix(api.URL, "http://")})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("wedged node: %v after %v", err, time.Since(start))
	}
}
