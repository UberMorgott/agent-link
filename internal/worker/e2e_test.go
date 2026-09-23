package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

	resp, err := httpGet(t, a.api.URL+"/wait?timeout=20s")
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

// The answering side (node and worker, like the tray app) is stopped while
// its detached agent runs and started again on the same state: the sender gets
// the full answer once, and the agent ran once.
func TestAnswerSurvivesDaemonRestart(t *testing.T) {
	runs := fakeRunLog(t)
	agent := fakeAgent(t, "slow-stream")
	agent.Format = FormatClaude
	lnA, lnB := listenTCP(t), listenTCP(t)
	addrB := lnB.Addr().String()
	a := startNode(t, "a", lnA, "b", lnB)
	a.serve(t)
	bData, bState := t.TempDir(), t.TempDir()

	// daemon starts node b and its worker; the returned func stops both.
	daemon := func(ln net.Listener) func() {
		cfg := config.Config{
			Node: "b", Listen: addrB, API: "127.0.0.1:0", DataDir: bData, SecretEnv: "UNUSED",
			Peers: []config.Peer{{Name: "a", Addr: lnA.Addr().String()}},
		}
		n, err := node.New(cfg, []byte(e2eSecret), nil)
		if err != nil {
			t.Fatal(err)
		}
		w, err := New(nil, n.SendMessage, bState, t.TempDir(), Options{Agent: func() Command { return agent }}, nil)
		if err != nil {
			t.Fatal(err)
		}
		n.SetInboundHook(w.Accept)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { n.Run(ctx, ln) })
		wg.Go(func() { w.Run(ctx) })
		var once sync.Once
		stop := func() { once.Do(func() { cancel(); wg.Wait() }) }
		t.Cleanup(stop)
		return stop
	}
	stop := daemon(lnB)
	req, err := a.n.Send("b", "the question", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent started", func() bool { return len(runs()) == 1 })
	stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", addrB)
	if err != nil {
		t.Fatal(err)
	}
	daemon(ln)

	resp, err := httpGet(t, a.api.URL+"/wait?timeout=30s")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []node.Message
	err = json.NewDecoder(resp.Body).Decode(&msgs)
	_ = resp.Body.Close()
	if err != nil || len(msgs) != 1 || msgs[0].ReplyTo != req.ID || msgs[0].JobStatus != node.JobCompleted || msgs[0].Body != "slow done: the question" {
		t.Fatalf("wait = %+v, err %v", msgs, err)
	}
	if r := runs(); len(r) != 1 {
		t.Fatalf("agent runs %v, want one", r)
	}
}

// Two real nodes and a chat: b's worker answers a's two questions in one
// agent session, the second turn sees a's informational message, and the chat
// is idle afterwards.
func TestChatTurnsBetweenNodes(t *testing.T) {
	agent, runs := chatAgent(t)
	lnA, lnB := listenTCP(t), listenTCP(t)
	a := startNode(t, "a", lnA, "b", lnB)
	b := startNode(t, "b", lnB, "a", lnA)
	w, err := New(nil, b.n.SendMessage, t.TempDir(), t.TempDir(),
		Options{Agent: func() Command { return agent }, Chats: b.n, Self: "b", ActivityEvery: 10 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.n.SetInboundHook(w.Accept)
	a.serve(t)
	b.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	var chat node.ChatInfo
	eventually(t, "chat created", func() bool {
		chat, err = a.n.CreateChat([]string{"b"}, "")
		return err == nil
	})
	replies := func() []node.ChatMessage {
		msgs, _ := a.n.ChatMessages(chat.ID, 0, 0, 100)
		var out []node.ChatMessage
		for _, m := range msgs {
			if m.From == "b" && m.Kind == "" && m.JobStatus == node.JobCompleted {
				out = append(out, m)
			}
		}
		return out
	}
	q1, err := a.n.SendChat(node.ChatSend{ChatID: chat.ID, Body: "first question", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "first reply", func() bool { return len(replies()) == 1 })
	if _, err := a.n.SendChat(node.ChatSend{ChatID: chat.ID, Body: "fyi for later"}); err != nil {
		t.Fatal(err)
	}
	q2, err := a.n.SendChat(node.ChatSend{ChatID: chat.ID, Body: "second question", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "second reply", func() bool { return len(replies()) == 2 })
	got := replies()
	if got[0].ReplyTo != q1.ID || got[1].ReplyTo != q2.ID || got[0].ID != node.DerivedID(q1.ID, "b/reply") ||
		!strings.Contains(got[1].Body, "a: fyi for later") || strings.Contains(got[1].Body, "first question") {
		t.Fatalf("replies %+v", got)
	}
	r := runs()
	if len(r) != 2 || strings.Split(r[0], ":")[1] != strings.Split(r[1], ":")[1] || !strings.HasPrefix(r[1], "resume:") {
		t.Fatalf("agent runs %v, want two turns of one session", r)
	}
	info, err := a.n.Chat(chat.ID)
	if err != nil || info.Active {
		t.Fatalf("chat %+v, %v", info, err)
	}
}

type e2eNode struct {
	n   *node.Node
	ln  net.Listener
	api *httptest.Server
}

// httpGet sends a GET bounded by the test's context.
func httpGet(t *testing.T, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
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

// A chat opened between two real nodes, then a long question whose end
// carries a marker: without a handler b answers the question with one held
// status that a sees; with one, exactly one job runs and its agent gets the
// whole text.
func TestChatQuestionReachesTheOtherSide(t *testing.T) {
	for _, handler := range []bool{false, true} {
		t.Run(map[bool]string{false: "no handler", true: "handler"}[handler], func(t *testing.T) {
			agent, runs := chatAgent(t)
			lnA, lnB := listenTCP(t), listenTCP(t)
			a := startNode(t, "a", lnA, "b", lnB)
			b := startNode(t, "b", lnB, "a", lnA)
			opt := Options{Chats: b.n, Self: "b"}
			if handler {
				opt.Agent = func() Command { return agent }
			}
			w, err := New(nil, b.n.SendMessage, t.TempDir(), t.TempDir(), opt, nil)
			if err != nil {
				t.Fatal(err)
			}
			if handler {
				b.n.SetInboundHook(w.Accept)
			} else {
				b.n.SetInboundHook(w.ChatsOnly)
			}
			a.serve(t)
			b.serve(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); w.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			var chat node.ChatInfo
			eventually(t, "chat created", func() bool { chat, err = a.n.CreateChat([]string{"b"}, ""); return err == nil })
			body := strings.Repeat("длинный вопрос ", 400) + "MARKER-END"
			q, err := a.n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: body, Ask: []string{"b"}})
			if err != nil {
				t.Fatal(err)
			}
			if !handler {
				// Nobody answers automatically: the question waits unread on b,
				// delivered as a sees it, with no held status.
				eventually(t, "a sees it delivered, unread", func() bool {
					msgs, _ := a.n.ChatMessages(chat.ID, 0, 0, 10)
					for _, m := range msgs {
						if m.ID == q.ID {
							return len(m.Delivery) == 1 && m.Delivery[0].State == node.StateDelivered
						}
					}
					return false
				})
				if page, err := b.n.Unread("", "", 10); err != nil || page.Total != 1 || page.Messages[0].ID != q.ID || !page.Messages[0].AsksYou {
					t.Fatalf("b's unread: %+v, %v", page, err)
				}
				if info, _ := a.n.Chat(chat.ID); info.Active || len(info.Members[1].Held) != 0 {
					t.Fatalf("a's view of b: %+v", info.Members)
				}
				if msgs, _ := b.n.ChatMessages(chat.ID, 0, 0, 10); len(msgs) != 2 || msgs[1].Body != body {
					t.Fatalf("b's copy of the chat: %+v", msgs)
				}
				return
			}
			eventually(t, "reply", func() bool {
				msgs, _ := a.n.ChatMessages(chat.ID, 0, 0, 10)
				for _, m := range msgs {
					if m.From == "b" && m.ReplyTo == q.ID && m.JobStatus == node.JobCompleted {
						if !strings.Contains(m.Body, body) {
							t.Fatalf("the agent did not get the whole question: %.200q", m.Body)
						}
						return true
					}
				}
				return false
			})
			if r := runs(); len(r) != 1 {
				t.Fatalf("agent runs %v, want one", r)
			}
		})
	}
}
