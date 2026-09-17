package worker

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// The test binary doubles as a fake agent CLI when AGENTLINK_FAKE_AGENT is set:
// "echo" prints "echo: <stdin>" (or writes it to the file after -o), "sleep"
// blocks, "fail" exits 3.
func TestMain(m *testing.M) {
	switch os.Getenv("AGENTLINK_FAKE_AGENT") {
	case "":
		os.Exit(m.Run())
	case "echo":
		in, _ := io.ReadAll(os.Stdin)
		out := "echo: " + string(in)
		if len(os.Args) > 2 && os.Args[1] == "-o" {
			_ = os.WriteFile(os.Args[2], []byte(out), 0o600)
			return
		}
		_, _ = os.Stdout.WriteString(out)
	case "sleep":
		time.Sleep(time.Minute)
	case "fail":
		_, _ = os.Stderr.WriteString("boom")
		os.Exit(3)
	}
}

func fakeAgent(t *testing.T, mode string, args ...string) Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTLINK_FAKE_AGENT", mode)
	return Command{Name: exe, Args: args}
}

type sent struct{ to, body, replyTo string }

type recorder struct {
	mu   sync.Mutex
	msgs []sent
	got  chan struct{}
}

func newRecorder() *recorder { return &recorder{got: make(chan struct{}, 100)} }

func (r *recorder) send(to, body, replyTo string) (node.Message, error) {
	r.mu.Lock()
	r.msgs = append(r.msgs, sent{to, body, replyTo})
	r.mu.Unlock()
	r.got <- struct{}{}
	return node.Message{}, nil
}

func (r *recorder) wait(t *testing.T, n int) []sent {
	t.Helper()
	for range n {
		select {
		case <-r.got:
		case <-time.After(20 * time.Second):
			t.Fatalf("timed out waiting for reply")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sent(nil), r.msgs...)
}

func start(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func msg(id, body string) node.Message {
	return node.Message{ID: id, From: "peer", To: "me", Body: body}
}

const id1 = "0123456789abcdef0123456789abcdef"
const id2 = "fedcba9876543210fedcba9876543210"

func TestSuccessRepliesToRequest(t *testing.T) {
	rec := newRecorder()
	w := New(fakeAgent(t, "echo").Runner(), rec.send, t.TempDir(), 0, nil)
	start(t, w)
	w.Offer(msg(id1, "first"))
	w.Offer(msg(id2, "second"))
	got := rec.wait(t, 2)
	want := []sent{{"peer", "echo: first", id1}, {"peer", "echo: second", id2}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reply %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestOutputFileArg(t *testing.T) {
	rec := newRecorder()
	w := New(fakeAgent(t, "echo", "-o", OutputFileArg).Runner(), rec.send, t.TempDir(), 0, nil)
	start(t, w)
	w.Offer(msg(id1, "via file"))
	if got := rec.wait(t, 1)[0]; got.body != "echo: via file" || got.replyTo != id1 {
		t.Fatalf("reply = %+v", got)
	}
}

func TestRepliesAreIgnored(t *testing.T) {
	rec := newRecorder()
	w := New(fakeAgent(t, "echo").Runner(), rec.send, t.TempDir(), 0, nil)
	start(t, w)
	reply := msg(id2, "an answer")
	reply.ReplyTo = id1
	w.Offer(reply)
	w.Offer(msg(id1, "request"))
	got := rec.wait(t, 1)
	select {
	case <-rec.got:
		t.Fatal("reply message triggered the handler")
	case <-time.After(300 * time.Millisecond):
	}
	if len(got) != 1 || got[0].replyTo != id1 {
		t.Fatalf("replies = %+v", got)
	}
}

func TestTimeout(t *testing.T) {
	rec := newRecorder()
	w := New(fakeAgent(t, "sleep").Runner(), rec.send, t.TempDir(), 500*time.Millisecond, nil)
	start(t, w)
	began := time.Now()
	w.Offer(msg(id1, "hang"))
	got := rec.wait(t, 1)[0]
	if !strings.Contains(got.body, "timed out") || got.replyTo != id1 {
		t.Fatalf("reply = %+v", got)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Fatalf("timeout reply took %s", d)
	}
}

func TestFailure(t *testing.T) {
	rec := newRecorder()
	w := New(fakeAgent(t, "fail").Runner(), rec.send, t.TempDir(), 0, nil)
	start(t, w)
	w.Offer(msg(id1, "x"))
	got := rec.wait(t, 1)[0]
	if !strings.Contains(got.body, "handler failed") || !strings.Contains(got.body, "boom") {
		t.Fatalf("reply = %+v", got)
	}
}

func TestSerial(t *testing.T) {
	var mu sync.Mutex
	running, maxRunning := 0, 0
	run := func(ctx context.Context, _, prompt string) (string, error) {
		mu.Lock()
		running++
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return prompt, nil
	}
	rec := newRecorder()
	w := New(run, rec.send, t.TempDir(), 0, nil)
	start(t, w)
	for _, id := range []string{id1, id2, id1, id2} {
		w.Offer(msg(id, "p"))
	}
	rec.wait(t, 4)
	if maxRunning != 1 {
		t.Fatalf("max concurrent jobs = %d, want 1", maxRunning)
	}
}
