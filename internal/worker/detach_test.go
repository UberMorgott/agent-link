package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// detachedWorker runs agent detached (Options.Agent) on state.
func detachedWorker(t *testing.T, agent Command, rec *recorder, state string, opt Options) *Worker {
	t.Helper()
	opt.Agent = func() Command { return agent }
	w, err := New(nil, rec.send, state, t.TempDir(), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// fakeRunLog points the fake agents' run log at a fresh file and returns a
// reader of its lines.
func fakeRunLog(t *testing.T) func() []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runs.log")
	t.Setenv("AGENTLINK_FAKE_LOG", p)
	return func() []string {
		data, _ := os.ReadFile(filepath.Clean(p))
		return strings.Fields(string(data))
	}
}

// agentPID waits for the job's detached agent and returns its record; the
// agent is killed when the test ends.
func agentPID(t *testing.T, w *Worker, id string) Proc {
	t.Helper()
	var rec Proc
	eventually(t, "agent launched", func() bool {
		j, _ := w.Job(id)
		if j.Proc == nil {
			return false
		}
		rec = *j.Proc
		return true
	})
	t.Cleanup(func() { killLeftover(context.Background(), &rec) }) // t.Context() is done by cleanup time
	return rec
}

func alive(rec Proc) bool {
	_, ok := attach(rec.PID, rec.Start)
	return ok
}

// A request addressed to an area with a project runs in that project's
// directory; any other area keeps the work directory. Every request runs the
// same (full-capability) command.
func TestProjectAreaPicksDir(t *testing.T) {
	project := t.TempDir()
	for _, c := range []struct {
		name, area string
		mapped     bool
	}{
		{"mapped area", "dev", true},
		{"unmapped area", "other", false},
		{"no area", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := newRecorder()
			agent := fakeAgent(t, "echo")
			workDir := t.TempDir()
			var mu sync.Mutex
			asked := 0
			opt := Options{
				Agent: func() Command {
					mu.Lock()
					asked++
					mu.Unlock()
					return agent
				},
				Project: func(area string) string {
					if area == "dev" {
						return project
					}
					return ""
				},
			}
			w, err := New(nil, rec.send, t.TempDir(), workDir, opt, nil)
			if err != nil {
				t.Fatal(err)
			}
			start(t, w)
			m := msg(id1, "q")
			m.Area = c.area
			accept(t, w, m)
			if got := rec.wait(t, 1)[0]; got.JobStatus != node.JobCompleted {
				t.Fatalf("reply = %+v", got)
			}
			j, _ := w.Job(id1)
			wantDir := workDir
			if c.mapped {
				wantDir = project
			}
			if j.Proc == nil || j.Proc.Dir != filepath.Clean(wantDir) {
				t.Fatalf("proc = %+v, want dir %q", j.Proc, wantDir)
			}
			mu.Lock()
			defer mu.Unlock()
			if asked != 1 {
				t.Fatalf("Agent asked %d times, want 1", asked)
			}
		})
	}
}
func TestDetachedRunReplies(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  Command
		want string
	}{
		{"stdout", fakeAgent(t, "echo"), "echo: q"},
		{"output file", fakeAgent(t, "echo", "-o", OutputFileArg), "echo: q"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := newRecorder()
			state := t.TempDir()
			w := detachedWorker(t, c.cmd, rec, state, Options{})
			start(t, w)
			accept(t, w, msg(id1, "q"))
			if got := rec.wait(t, 1)[0]; got.Body != c.want || got.JobStatus != node.JobCompleted {
				t.Fatalf("reply = %+v", got)
			}
			if _, err := os.Stat(filepath.Join(state, "jobs", id1)); !os.IsNotExist(err) {
				t.Fatalf("run files of a completed job left: %v", err)
			}
		})
	}
}

func TestDetachedFailure(t *testing.T) {
	rec := newRecorder()
	w := detachedWorker(t, fakeAgent(t, "fail"), rec, t.TempDir(), Options{})
	start(t, w)
	accept(t, w, msg(id1, "x"))
	got := rec.wait(t, 1)[0]
	if !strings.Contains(got.Body, "handler failed") || !strings.Contains(got.Body, "boom") || got.JobStatus != node.JobFailed {
		t.Fatalf("reply = %+v", got)
	}
}

// The idle timeout still kills a detached agent.
func TestDetachedIdleTimeoutKills(t *testing.T) {
	rec := newRecorder()
	stall := fakeAgent(t, "stream-stall")
	stall.Format = FormatClaude
	w := detachedWorker(t, stall, rec, t.TempDir(), Options{IdleTimeout: 700 * time.Millisecond})
	start(t, w)
	accept(t, w, msg(id1, "stall"))
	p := agentPID(t, w, id1)
	if got := rec.wait(t, 1)[0]; !strings.HasPrefix(got.Body, "agentlink: "+ErrIdle) {
		t.Fatalf("reply = %+v", got)
	}
	eventually(t, "agent killed", func() bool { return !alive(p) })
}

// Cancel kills a running detached agent and fails the job.
func TestCancelKillsDetachedAgent(t *testing.T) {
	rec := newRecorder()
	w := detachedWorker(t, fakeAgent(t, "sleep"), rec, t.TempDir(), Options{})
	start(t, w)
	accept(t, w, msg(id1, "x"))
	p := agentPID(t, w, id1)
	eventually(t, "watched", func() bool { return w.Cancel(id1) })
	if got := rec.wait(t, 1)[0]; got.Body != "agentlink: "+ErrCancelled || got.JobStatus != node.JobFailed {
		t.Fatalf("reply = %+v", got)
	}
	eventually(t, "agent killed", func() bool { return !alive(p) })
}

// completedStatus reports whether a completed status (no final reply) was sent for id.
func completedStatus(rec *recorder, id string) bool {
	return len(rec.filter(func(m node.Message) bool {
		return m.Kind == node.KindStatus && m.ReplyTo == id && m.JobStatus == node.JobCompleted
	})) == 1
}

// A request answered on this node another way kills its running detached
// agent; the job completes without a reply and without a failure.
func TestAnsweredStopsDetachedAgent(t *testing.T) {
	rec := newRecorder()
	w := detachedWorker(t, fakeAgent(t, "sleep"), rec, t.TempDir(), Options{})
	start(t, w)
	accept(t, w, msg(id1, "x"))
	p := agentPID(t, w, id1)
	if !w.Answered(id1) {
		t.Fatal("Answered on a running job = false")
	}
	eventually(t, "agent killed", func() bool { return !alive(p) })
	eventually(t, "job completed", func() bool { j, _ := w.Job(id1); return j.Status == node.JobCompleted && j.Replied })
	if j, _ := w.Job(id1); !j.Answered || j.Error != ErrAnswered || j.Result != "" {
		t.Fatalf("job = %+v", j)
	}
	rec.quiet(t)
	if !completedStatus(rec, id1) {
		t.Fatalf("no completed status: %+v", rec.filter(func(node.Message) bool { return true }))
	}
	if w.Answered(id1) {
		t.Fatal("Answered on a finished job = true")
	}
}

// A queued request answered on this node never runs; a running one is
// cancelled. Neither sends a failure.
func TestAnsweredQueuedAndRunning(t *testing.T) {
	rec := newRecorder()
	var mu sync.Mutex
	var prompts []string
	run := func(ctx context.Context, _, prompt string, _ func(string)) (string, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		<-ctx.Done()
		return "", ctx.Err()
	}
	w, err := New(run, rec.send, t.TempDir(), t.TempDir(), Options{MaxJobs: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start(t, w)
	accept(t, w, msg(id1, "first"))
	eventually(t, "first running", func() bool { mu.Lock(); defer mu.Unlock(); return len(prompts) == 1 })
	accept(t, w, msg(id2, "second"))
	for _, id := range []string{id2, id1} {
		if !w.Answered(id) {
			t.Fatalf("Answered(%s) = false", id)
		}
		eventually(t, "job completed", func() bool { j, _ := w.Job(id); return j.Status == node.JobCompleted && j.Replied })
		if !completedStatus(rec, id) {
			t.Fatalf("no completed status for %s", id)
		}
	}
	rec.quiet(t)
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 1 || prompts[0] != "first" {
		t.Fatalf("runs = %q", prompts)
	}
}

// Stopping the worker (an app restart or update) leaves the agent running; the
// next worker reattaches, relays its activity and delivers the full answer.
// The agent runs once.
func TestAgentSurvivesRestart(t *testing.T) {
	runs := fakeRunLog(t)
	agent := fakeAgent(t, "slow-stream")
	agent.Format = FormatClaude
	rec := newRecorder()
	state := t.TempDir()
	opt := Options{ActivityEvery: 50 * time.Millisecond}
	w1 := detachedWorker(t, agent, rec, state, opt)
	stop := start(t, w1)
	accept(t, w1, msg(id1, "q"))
	p := agentPID(t, w1, id1)
	eventually(t, "first activity", func() bool {
		return len(rec.filter(func(m node.Message) bool { return m.Activity != "" })) > 0
	})
	stop()
	if !alive(p) {
		t.Fatal("stopping the worker killed the agent")
	}
	before := len(rec.filter(func(m node.Message) bool { return m.Activity != "" }))
	w2 := detachedWorker(t, agent, rec, state, opt)
	start(t, w2)
	got := rec.wait(t, 1)[0]
	if got.Body != "slow done: q" || got.JobStatus != node.JobCompleted {
		t.Fatalf("reply = %+v", got)
	}
	if r := runs(); len(r) != 1 {
		t.Fatalf("agent runs %v, want one start", r)
	}
	after := rec.filter(func(m node.Message) bool { return m.Activity != "" })
	if len(after) <= before {
		t.Fatal("no activity relayed after the reattach")
	}
	seen := map[string]bool{}
	for _, m := range after {
		if seen[m.ID] {
			t.Fatalf("activity id reused after the reattach: %+v", m)
		}
		seen[m.ID] = true
	}
	if j, _ := w2.Job(id1); j.Attempts != 1 {
		t.Fatalf("job = %+v", j)
	}
}

// An agent that finished while no app was running is finalized from its
// output, not run again.
func TestAgentFinishedWhileAway(t *testing.T) {
	runs := fakeRunLog(t)
	agent := fakeAgent(t, "slow-stream")
	agent.Format = FormatClaude
	rec := newRecorder()
	state := t.TempDir()
	w1 := detachedWorker(t, agent, rec, state, Options{})
	stop := start(t, w1)
	accept(t, w1, msg(id1, "q"))
	p := agentPID(t, w1, id1)
	stop()
	eventually(t, "agent done", func() bool { return !alive(p) })
	w2 := detachedWorker(t, agent, rec, state, Options{})
	start(t, w2)
	if got := rec.wait(t, 1)[0]; got.Body != "slow done: q" || got.JobStatus != node.JobCompleted {
		t.Fatalf("reply = %+v", got)
	}
	if r := runs(); len(r) != 1 {
		t.Fatalf("agent runs %v", r)
	}
}

// An agent killed mid-run while the app was down (a reboot) is resumed in its
// session, not started over.
func TestDeadAgentIsResumed(t *testing.T) {
	runs := fakeRunLog(t)
	agent := fakeAgent(t, "resumable")
	agent.Format = FormatClaude
	agent.SessionArgs = []string{"--session-id", SessionIDArg}
	agent.ResumeArgs = []string{"--resume", SessionIDArg}
	rec := newRecorder()
	state := t.TempDir()
	w1 := detachedWorker(t, agent, rec, state, Options{})
	stop := start(t, w1)
	accept(t, w1, msg(id1, "q"))
	p := agentPID(t, w1, id1)
	eventually(t, "session announced", func() bool {
		data, _ := os.ReadFile(w1.runBase(id1, 1) + ".out")
		return strings.Contains(string(data), p.Session)
	})
	stop()
	if err := killTree(t.Context(), p.PID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent dead", func() bool { return !alive(p) })
	w2 := detachedWorker(t, agent, rec, state, Options{})
	start(t, w2)
	got := rec.wait(t, 1)[0]
	if got.Body != "resumed "+p.Session+": "+strings.TrimSpace(ResumePrompt) || got.JobStatus != node.JobCompleted {
		t.Fatalf("reply = %+v", got)
	}
	if r := runs(); strings.Join(r, ",") != "start,resume" {
		t.Fatalf("agent runs %v", r)
	}
	if j, _ := w2.Job(id1); j.Attempts != 2 || j.Proc == nil || !j.Proc.Resumed {
		t.Fatalf("job = %+v", j)
	}
}

// Without a session to resume, an agent that died mid-run starts over once.
func TestDeadAgentWithoutSessionStartsOver(t *testing.T) {
	rec := newRecorder()
	state := t.TempDir()
	w1 := detachedWorker(t, fakeAgent(t, "sleep"), rec, state, Options{})
	stop := start(t, w1)
	accept(t, w1, msg(id1, "q"))
	p := agentPID(t, w1, id1)
	stop()
	if err := killTree(t.Context(), p.PID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent dead", func() bool { return !alive(p) })
	w2 := detachedWorker(t, fakeAgent(t, "echo"), rec, state, Options{})
	start(t, w2)
	if got := rec.wait(t, 1)[0]; got.Body != "echo: q" || got.JobStatus != node.JobCompleted {
		t.Fatalf("reply = %+v", got)
	}
	if j, _ := w2.Job(id1); j.Attempts != 2 {
		t.Fatalf("job = %+v", j)
	}
}
