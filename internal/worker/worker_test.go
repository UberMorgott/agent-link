package worker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	case "tree":
		// A long-lived child, like the node process behind an agent's .cmd shim.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "AGENTLINK_FAKE_AGENT=sleep")
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		_ = os.WriteFile(os.Getenv("AGENTLINK_FAKE_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
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

// recorder captures sent messages; final replies (not status updates) signal got.
type recorder struct {
	mu   sync.Mutex
	msgs []node.Message
	got  chan struct{}
}

func newRecorder() *recorder { return &recorder{got: make(chan struct{}, 100)} }

func (r *recorder) send(m node.Message) (node.Message, error) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	if m.Kind == "" {
		r.got <- struct{}{}
	}
	return m, nil
}

// wait blocks for n more final replies and returns all final replies so far.
func (r *recorder) wait(t *testing.T, n int) []node.Message {
	t.Helper()
	for range n {
		select {
		case <-r.got:
		case <-time.After(20 * time.Second):
			t.Fatalf("timed out waiting for reply")
		}
	}
	return r.filter(func(m node.Message) bool { return m.Kind == "" })
}

func (r *recorder) filter(keep func(node.Message) bool) []node.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []node.Message
	for _, m := range r.msgs {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

func (r *recorder) quiet(t *testing.T) {
	t.Helper()
	select {
	case <-r.got:
		t.Fatal("unexpected reply")
	case <-time.After(300 * time.Millisecond):
	}
}

func newWorker(t *testing.T, run Runner, rec *recorder, state string, timeout time.Duration) *Worker {
	t.Helper()
	w, err := New(run, rec.send, state, t.TempDir(), timeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// start runs w and returns a stop function that cancels it and waits.
func start(t *testing.T, w *Worker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func accept(t *testing.T, w *Worker, m node.Message) {
	t.Helper()
	if err := w.Accept(m); err != nil {
		t.Fatal(err)
	}
}

func msg(id, body string) node.Message {
	return node.Message{ID: id, From: "peer", To: "me", Body: body}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const id1 = "0123456789abcdef0123456789abcdef"
const id2 = "fedcba9876543210fedcba9876543210"
const id3 = "00000000000000000000000000000003"

func TestSuccessRepliesToRequest(t *testing.T) {
	rec := newRecorder()
	w := newWorker(t, fakeAgent(t, "echo").Runner(), rec, t.TempDir(), 0)
	start(t, w)
	accept(t, w, msg(id1, "first"))
	accept(t, w, msg(id2, "second"))
	got := rec.wait(t, 2)
	want := []struct{ body, replyTo string }{{"echo: first", id1}, {"echo: second", id2}}
	for i := range want {
		if got[i].To != "peer" || got[i].Body != want[i].body || got[i].ReplyTo != want[i].replyTo || got[i].JobStatus != node.JobCompleted {
			t.Fatalf("reply %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestOutputFileArg(t *testing.T) {
	rec := newRecorder()
	w := newWorker(t, fakeAgent(t, "echo", "-o", OutputFileArg).Runner(), rec, t.TempDir(), 0)
	start(t, w)
	accept(t, w, msg(id1, "via file"))
	if got := rec.wait(t, 1)[0]; got.Body != "echo: via file" || got.ReplyTo != id1 {
		t.Fatalf("reply = %+v", got)
	}
}

// Replies and status updates never create jobs.
func TestRepliesAndStatusUpdatesAreIgnored(t *testing.T) {
	rec := newRecorder()
	var runs atomic.Int32
	run := func(context.Context, string, string) (string, error) { runs.Add(1); return "ok", nil }
	w := newWorker(t, run, rec, t.TempDir(), 0)
	start(t, w)
	reply := msg(id2, "an answer")
	reply.ReplyTo = id1
	accept(t, w, reply)
	status := node.Message{ID: id3, From: "peer", To: "me", ReplyTo: id1, Kind: node.KindStatus, JobStatus: node.JobQueued}
	accept(t, w, status)
	rec.quiet(t)
	if _, ok := w.Job(id2); ok {
		t.Fatal("reply created a job")
	}
	if _, ok := w.Job(id3); ok {
		t.Fatal("status update created a job")
	}
	if sent := len(rec.filter(func(node.Message) bool { return true })); runs.Load() != 0 || sent != 0 {
		t.Fatalf("runs = %d, sent = %d", runs.Load(), sent)
	}
}

func TestStatusSequence(t *testing.T) {
	rec := newRecorder()
	w := newWorker(t, fakeAgent(t, "echo").Runner(), rec, t.TempDir(), 0)
	start(t, w)
	accept(t, w, msg(id1, "x"))
	rec.wait(t, 1)
	var seq []string
	for _, m := range rec.filter(func(node.Message) bool { return true }) {
		if m.ReplyTo != id1 {
			t.Fatalf("message not about the request: %+v", m)
		}
		seq = append(seq, m.Kind+":"+m.JobStatus)
	}
	want := []string{"status:queued", "status:running", ":completed"}
	if !slices.Equal(seq, want) {
		t.Fatalf("sequence %v, want %v", seq, want)
	}
}

func TestTimeoutFailsWithoutRetry(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	w := newWorker(t, fakeAgent(t, "sleep").Runner(), rec, state, 500*time.Millisecond)
	stop := start(t, w)
	began := time.Now()
	accept(t, w, msg(id1, "hang"))
	got := rec.wait(t, 1)[0]
	if !strings.Contains(got.Body, "timed out") || got.ReplyTo != id1 || got.JobStatus != node.JobFailed {
		t.Fatalf("reply = %+v", got)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Fatalf("timeout reply took %s", d)
	}
	stop()
	// A restart does not run the failed job again.
	w2 := newWorker(t, fakeAgent(t, "echo").Runner(), rec, state, 0)
	start(t, w2)
	rec.quiet(t)
	if j, _ := w2.Job(id1); j.Status != node.JobFailed || j.Attempts != 1 || !j.Replied {
		t.Fatalf("job = %+v", j)
	}
}

func TestFailure(t *testing.T) {
	rec := newRecorder()
	w := newWorker(t, fakeAgent(t, "fail").Runner(), rec, t.TempDir(), 0)
	start(t, w)
	accept(t, w, msg(id1, "x"))
	got := rec.wait(t, 1)[0]
	if !strings.Contains(got.Body, "handler failed") || !strings.Contains(got.Body, "boom") || got.JobStatus != node.JobFailed {
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
	w := newWorker(t, run, rec, t.TempDir(), 0)
	start(t, w)
	for _, id := range []string{id1, id2, id3} {
		accept(t, w, msg(id, "p"))
	}
	rec.wait(t, 3)
	mu.Lock()
	defer mu.Unlock()
	if maxRunning != 1 {
		t.Fatalf("max concurrent jobs = %d, want 1", maxRunning)
	}
}

// A duplicate request id never runs twice: not while queued, not after it
// completed, not after a restart.
func TestDuplicateRequestRunsOnce(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	var runs atomic.Int32
	run := func(context.Context, string, string) (string, error) { runs.Add(1); return "done", nil }
	w := newWorker(t, run, rec, state, 0)
	accept(t, w, msg(id1, "x"))
	accept(t, w, msg(id1, "x"))
	stop := start(t, w)
	rec.wait(t, 1)
	accept(t, w, msg(id1, "x"))
	rec.quiet(t)
	stop()
	w2 := newWorker(t, run, rec, state, 0)
	start(t, w2)
	accept(t, w2, msg(id1, "x"))
	rec.quiet(t)
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", runs.Load())
	}
	if n := len(rec.filter(func(m node.Message) bool { return m.JobStatus == node.JobQueued })); n != 1 {
		t.Fatalf("queued updates = %d, want 1", n)
	}
}

// Accepted but never started (a crash right after the ACK): the next start
// runs the jobs in acceptance order.
func TestCrashBeforeRunResumes(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	run := func(_ context.Context, _, prompt string) (string, error) { return "did " + prompt, nil }
	w := newWorker(t, run, rec, state, 0)
	for i, id := range []string{id2, id1, id3} {
		accept(t, w, msg(id, strconv.Itoa(i)))
	}
	// w never runs: the process died.
	w2 := newWorker(t, run, rec, state, 0)
	start(t, w2)
	got := rec.wait(t, 3)
	for i, id := range []string{id2, id1, id3} {
		if got[i].ReplyTo != id || got[i].Body != "did "+strconv.Itoa(i) || got[i].JobStatus != node.JobCompleted {
			t.Fatalf("reply %d = %+v", i, got[i])
		}
	}
}

// An interrupted job (quit, settings save) runs once more on the next start;
// interrupted again, it fails with a reply and is not run a third time.
func TestInterruptRetriesOnceThenFails(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	var runs atomic.Int32
	run := func(ctx context.Context, _, _ string) (string, error) {
		runs.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	for attempt := int32(1); attempt <= MaxAttempts; attempt++ {
		w := newWorker(t, run, rec, state, 0)
		if attempt == 1 {
			accept(t, w, msg(id1, "x"))
		}
		stop := start(t, w)
		eventually(t, "attempt "+strconv.Itoa(int(attempt)), func() bool { return runs.Load() == attempt })
		stop()
		if j, _ := w.Job(id1); j.Status != node.JobRunning || j.Attempts != int(attempt) {
			t.Fatalf("after interruption %d: job = %+v", attempt, j)
		}
	}
	if n := len(rec.wait(t, 0)); n != 0 {
		t.Fatalf("interruption replied %d times", n)
	}
	w := newWorker(t, run, rec, state, 0)
	start(t, w)
	got := rec.wait(t, 1)
	if len(got) != 1 || got[0].JobStatus != node.JobFailed || got[0].Body != "agentlink: "+ErrInterrupted {
		t.Fatalf("replies = %+v", got)
	}
	rec.quiet(t)
	if runs.Load() != MaxAttempts {
		t.Fatalf("runs = %d, want %d", runs.Load(), MaxAttempts)
	}
}

// Jobs left queued when the handler is switched to none fail with a reply.
func TestNoHandlerFailsPendingJobs(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	w := newWorker(t, fakeAgent(t, "echo").Runner(), rec, state, 0)
	accept(t, w, msg(id1, "x"))
	none := newWorker(t, nil, rec, state, 0)
	start(t, none)
	got := rec.wait(t, 1)[0]
	if got.JobStatus != node.JobFailed || got.Body != "agentlink: "+ErrNoHandler || got.ReplyTo != id1 {
		t.Fatalf("reply = %+v", got)
	}
}

// A crash after the outcome was recorded but before the reply was queued:
// the next start sends the reply under the same id, without re-running.
func TestUnsentReplyIsResent(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	w := newWorker(t, nil, rec, state, 0)
	accept(t, w, msg(id1, "x"))
	j := w.jobs[id1]
	j.Status, j.Result = node.JobCompleted, "stored answer"
	if err := w.save(j); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	run := func(context.Context, string, string) (string, error) { runs.Add(1); return "again", nil }
	w2 := newWorker(t, run, rec, state, 0)
	start(t, w2)
	got := rec.wait(t, 1)[0]
	if got.Body != "stored answer" || got.ID != node.DerivedID(id1, "reply") || runs.Load() != 0 {
		t.Fatalf("reply = %+v, runs = %d", got, runs.Load())
	}
}
