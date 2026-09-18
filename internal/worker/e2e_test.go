package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
)

const e2eSecret = "test-secret-0123456789abcdef"

// Two real nodes: the sender sees queued, running, the handler's activity and a
// completed reply for its request, its inbox entry carries the latest status and the answer, and
// wait returns only the final reply.
func TestSenderObservesStatusSequence(t *testing.T) {
	lnA, lnB := listenTCP(t), listenTCP(t)
	a := startNode(t, "a", lnA, "b", lnB)
	b := startNode(t, "b", lnB, "a", lnA)

	release := make(chan struct{})
	run := func(ctx context.Context, _, prompt string, progress func(string)) (string, error) {
		progress("Read docs/index.md")
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "answer to " + prompt, nil
	}
	w, err := New(run, b.n.SendMessage, t.TempDir(), t.TempDir(), Options{ActivityEvery: 20 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.n.SetInboundHook(w.Accept)
	var mu sync.Mutex
	ids := map[string]bool{}
	var statuses []string
	a.n.SetInboundHook(func(m node.Message) error { // also sees resent duplicates
		mu.Lock()
		defer mu.Unlock()
		if !ids[m.ID] {
			ids[m.ID] = true
			statuses = append(statuses, m.Kind+":"+m.JobStatus+":"+m.Activity)
		}
		return nil
	})
	a.serve(t)
	b.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	req, err := a.n.Send("b", "the question", "")
	if err != nil {
		t.Fatal(err)
	}
	entry := func() node.Entry {
		entries, _ := a.n.Recent(0)
		for _, e := range entries {
			if e.Direction == "out" && e.ID == req.ID {
				return e
			}
		}
		return node.Entry{}
	}
	eventually(t, "activity shown to the sender", func() bool {
		e := entry()
		return e.JobStatus == node.JobRunning && e.Activity == "Read docs/index.md"
	})
	close(release)

	resp, err := http.Get(a.api.URL + "/wait?timeout=20s")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []node.Message
	err = json.NewDecoder(resp.Body).Decode(&msgs)
	_ = resp.Body.Close()
	if err != nil || len(msgs) != 1 || msgs[0].ReplyTo != req.ID || msgs[0].JobStatus != node.JobCompleted || msgs[0].Body != "answer to the question" {
		t.Fatalf("wait = %+v, err %v", msgs, err)
	}
	if e := entry(); e.JobStatus != node.JobCompleted || e.Answer != "answer to the question" {
		t.Fatalf("inbox entry = %+v", e)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"status:queued:", "status:running:", "status:running:Read docs/index.md", ":completed:"}; !slices.Equal(statuses, want) {
		t.Fatalf("sender saw %v, want %v", statuses, want)
	}
}

type e2eNode struct {
	n   *node.Node
	ln  net.Listener
	api *httptest.Server
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func startNode(t *testing.T, name string, ln net.Listener, peer string, peerLn net.Listener) *e2eNode {
	t.Helper()
	cfg := config.Config{
		Node: name, Listen: ln.Addr().String(), API: "127.0.0.1:0", DataDir: t.TempDir(), SecretEnv: "UNUSED",
		Peers: []config.Peer{{Name: peer, Addr: peerLn.Addr().String()}},
	}
	n, err := node.New(cfg, []byte(e2eSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &e2eNode{n: n, ln: ln}
}

func (e *e2eNode) serve(t *testing.T) {
	t.Helper()
	e.api = httptest.NewServer(e.n.APIHandler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.n.Run(ctx, e.ln) }()
	t.Cleanup(func() { cancel(); <-done; e.api.Close() })
}
