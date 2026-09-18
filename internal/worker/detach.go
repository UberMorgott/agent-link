package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A detached run (Options.Agent) survives a restart of this app. The agent
// is started in its own process group, outside the app's job object, with
// stdin, stdout and stderr on files in <jobs>/<id>/ (<attempt>.in, .out,
// .err, .last for the last-message file), so nothing ties it to the app. The
// job records the pid, the process start time (a reused pid is not the same
// process), the agent session id and how much stdout was relayed. The app
// tails stdout for activity and the answer. After a restart it reattaches to a
// process that is still running; a process that ended meanwhile is finalized
// from its output when that holds the end of the run, and otherwise resumed
// (ResumeArgs with the session id) or, without a session, started over once.

// Proc is the durable record of a detached agent run.
type Proc struct {
	PID   int    `json:"pid"`
	Start int64  `json:"start"` // OS process start time; guards against pid reuse
	Name  string `json:"name"`
	// Attempt names the run files <jobs>/<id>/<attempt>.*.
	Attempt int    `json:"attempt"`
	Format  string `json:"format,omitempty"`
	Dir     string `json:"dir"`
	// LastMessage: the answer is in the .last file (OutputFileArg).
	LastMessage bool `json:"last_message,omitempty"`
	// Session is the agent session id: chosen at launch (SessionArgs) or
	// announced on stdout; a resume continues it.
	Session string `json:"session,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	// Offset is how many stdout bytes were already relayed as activity.
	Offset int64 `json:"offset"`
	// Watches counts watches (1: the one after launch), so activity ids stay unique.
	Watches int `json:"watches"`
}

// proc is a detached agent process being watched.
type proc struct {
	pid   int
	start int64
	done  chan struct{} // closed when the process exited
	code  int           // exit code once done; -1 when unknown
}

// errCancelled marks a run stopped by Cancel.
var errCancelled = errors.New("cancelled")

// ErrCancelled is the failure text of a cancelled job.
const ErrCancelled = "cancelled"

func (w *Worker) runBase(id string, attempt int) string {
	return filepath.Join(w.jobsDir, id, strconv.Itoa(attempt))
}

// launch starts c detached for j's current attempt. resume continues session
// instead of sending the request again.
func (w *Worker) launch(ctx context.Context, j *Job, c Command, session string, resume bool) (*proc, error) {
	w.mu.Lock()
	id, attempt, prompt := j.Request.ID, j.Attempts, j.Request.Body
	w.mu.Unlock()
	base := w.runBase(id, attempt)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return nil, err
	}
	args, stdin := c.Args, c.Preamble+prompt
	switch {
	case resume:
		args, stdin = c.ResumeArgs, ResumePrompt
	case len(c.SessionArgs) > 0:
		session = newUUID()
		args = append(append([]string(nil), args...), c.SessionArgs...)
	default:
		session = ""
	}
	rec := Proc{Name: c.Name, Attempt: attempt, Format: c.Format, Dir: filepath.Clean(w.dir), Session: session, Resumed: resume}
	args = append([]string(nil), args...)
	for i, a := range args {
		switch a {
		case OutputFileArg:
			args[i], rec.LastMessage = base+".last", true
		case SessionIDArg:
			args[i] = session
		}
	}
	if err := os.WriteFile(base+".in", []byte(stdin), 0o600); err != nil {
		return nil, err
	}
	files := make([]*os.File, 0, 3)
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for _, spec := range []struct {
		ext  string
		flag int
	}{{".in", os.O_RDONLY}, {".out", os.O_CREATE | os.O_TRUNC | os.O_WRONLY}, {".err", os.O_CREATE | os.O_TRUNC | os.O_WRONLY}} {
		f, err := os.OpenFile(filepath.Clean(base+spec.ext), spec.flag, 0o600)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	start := func(breakaway bool) (*exec.Cmd, error) {
		// The run outlives this app (see detach): ctx must not end it.
		cmd := exec.CommandContext(context.WithoutCancel(ctx), c.Name, args...) //nolint:gosec // G204: the agent program the user configured, argv without a shell
		cmd.Dir = rec.Dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = files[0], files[1], files[2]
		detach(cmd, breakaway)
		return cmd, cmd.Start()
	}
	cmd, err := start(true)
	if err != nil && breakawayDenied(err) {
		cmd, err = start(false)
	}
	if err != nil {
		return nil, err
	}
	// cmd.Process holds the process handle, so the pid cannot be reused yet.
	rec.PID = cmd.Process.Pid
	rec.Start, err = procStart(rec.PID)
	if err != nil {
		w.log.Warn("agent start time", "id", id, "err", err)
	}
	p := &proc{pid: rec.PID, start: rec.Start, done: make(chan struct{}), code: -1}
	go func() {
		defer close(p.done)
		_ = cmd.Wait()
		if cmd.ProcessState != nil {
			p.code = cmd.ProcessState.ExitCode()
		}
	}()
	w.mu.Lock()
	j.Proc = &rec
	err = w.save(j)
	w.mu.Unlock()
	if err != nil {
		w.log.Error("save job", "id", id, "err", err)
	}
	w.log.Info("agent launched", "id", id, "pid", rec.PID, "attempt", attempt, "resume", resume)
	return p, nil
}

// handleDetached runs a claimed job as a detached agent, or, with reattach,
// picks up the run a previous app left behind.
func (w *Worker) handleDetached(ctx context.Context, j *Job, reattach bool) {
	if !reattach {
		p, err := w.launch(ctx, j, w.opt.Agent(), "", false)
		if err != nil {
			w.finish(j, node.JobFailed, "", fmt.Sprintf("handler failed: %v", err))
			return
		}
		w.watch(ctx, j, p)
		return
	}
	w.mu.Lock()
	rec, id := *j.Proc, j.Request.ID
	w.mu.Unlock()
	if p, ok := attach(rec.PID, rec.Start); ok {
		w.log.Info("agent reattached", "id", id, "pid", rec.PID)
		w.watch(ctx, j, p)
		return
	}
	// The agent ended while this app was not running.
	s := w.replay(id, rec)
	if s.finished || (rec.LastMessage && w.lastMessage(id, rec) != "") {
		w.log.Info("agent finished while away", "id", id, "pid", rec.PID)
		w.conclude(j, rec, s, -1)
		return
	}
	w.mu.Lock()
	attempts := j.Attempts
	w.mu.Unlock()
	if attempts >= MaxAttempts {
		w.log.Warn("agent interrupted again, failing", "id", id, "attempts", attempts)
		w.finish(j, node.JobFailed, "", ErrInterrupted)
		return
	}
	c := w.opt.Agent()
	resume := s.session != "" && len(c.ResumeArgs) > 0
	w.log.Info("agent died mid-run", "id", id, "pid", rec.PID, "resume", resume, "session", s.session)
	w.mu.Lock()
	j.Attempts, j.StartedAt = j.Attempts+1, time.Now().UTC()
	err := w.save(j)
	w.mu.Unlock()
	if err != nil {
		w.log.Error("save job", "id", id, "err", err)
		return
	}
	p, err := w.launch(ctx, j, c, s.session, resume)
	if err != nil {
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler failed: %v", err))
		return
	}
	w.watch(ctx, j, p)
}

// replay parses a run's whole stdout without relaying anything.
func (w *Worker) replay(id string, rec Proc) *stream {
	s := &stream{format: rec.Format, dir: rec.Dir}
	if f, err := os.Open(w.runBase(id, rec.Attempt) + ".out"); err == nil {
		_, _ = io.Copy(s, f)
		_ = f.Close()
	}
	s.flush()
	return s
}

func (w *Worker) lastMessage(id string, rec Proc) string {
	data, _ := os.ReadFile(w.runBase(id, rec.Attempt) + ".last")
	return strings.TrimSpace(string(data))
}

// watch follows a detached run until it ends, times out, is cancelled, or
// ctx is cancelled: then the agent keeps running and the job stays running on
// disk for the next start to reattach.
func (w *Worker) watch(ctx context.Context, j *Job, p *proc) {
	w.mu.Lock()
	m, attempt, started := j.Request, j.Attempts, j.StartedAt
	j.Proc.Watches++
	rec := *j.Proc
	if err := w.save(j); err != nil {
		w.log.Error("save job", "id", m.ID, "err", err)
	}
	cancelled := make(chan struct{})
	w.live[m.ID] = cancelled
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.live, m.ID)
		w.mu.Unlock()
	}()
	if rec.Watches == 1 {
		w.status(m, node.JobRunning, "running-"+strconv.Itoa(attempt), "")
	}
	prefix := "activity-" + strconv.Itoa(attempt)
	if rec.Watches > 1 {
		prefix += "-w" + strconv.Itoa(rec.Watches)
	}
	relay := w.relay(m, prefix)
	defer relay.stop()

	s := &stream{format: rec.Format, dir: rec.Dir}
	offset := int64(0)
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	idle := time.NewTimer(w.opt.IdleTimeout)
	defer idle.Stop()
	hard := time.NewTimer(time.Until(started.Add(w.opt.Timeout)))
	defer hard.Stop()
	read := func() {
		if f == nil {
			var err error
			if f, err = os.Open(w.runBase(m.ID, rec.Attempt) + ".out"); err != nil {
				f = nil
				return
			}
			// What an earlier watch relayed only rebuilds the parser state.
			n, _ := io.CopyN(s, f, rec.Offset)
			offset = n
			s.onLine = func(activity string) {
				idle.Reset(w.opt.IdleTimeout)
				relay.set(activity)
			}
		}
		n, _ := io.Copy(s, f)
		offset += n
	}
	saveOffset := func() {
		w.mu.Lock()
		if j.Proc != nil && j.Proc.Offset != offset {
			j.Proc.Offset = offset
			if err := w.save(j); err != nil {
				w.log.Error("save job", "id", m.ID, "err", err)
			}
		}
		w.mu.Unlock()
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	lastSave := time.Now()
	var stop error
	for stop == nil {
		read()
		if time.Since(lastSave) > 2*time.Second {
			saveOffset()
			lastSave = time.Now()
		}
		select {
		case <-p.done:
			read()
			s.flush()
			relay.stop()
			if f != nil {
				_ = f.Close() // before finish removes the run files
				f = nil
			}
			w.conclude(j, rec, s, p.code)
			return
		case <-ctx.Done():
			saveOffset()
			w.log.Info("app stopping, agent left running", "id", m.ID, "pid", p.pid)
			return
		case <-idle.C:
			stop = errIdleTimeout
		case <-hard.C:
			stop = errHardTimeout
		case <-cancelled:
			stop = errCancelled
		case <-tick.C:
		}
	}
	if err := killTree(ctx, p.pid); err != nil {
		w.log.Warn("kill agent", "id", m.ID, "pid", p.pid, "err", err)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
	relay.stop()
	switch {
	case errors.Is(stop, errIdleTimeout):
		w.finish(j, node.JobFailed, "", fmt.Sprintf("%s (нет активности %s)", ErrIdle, minutes(w.opt.IdleTimeout)))
	case errors.Is(stop, errHardTimeout):
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler timed out after %s", w.opt.Timeout))
	default:
		w.finish(j, node.JobFailed, "", ErrCancelled)
	}
}

// conclude finishes a run that ended by itself. code is the exit code, -1
// when unknown.
func (w *Worker) conclude(j *Job, rec Proc, s *stream, code int) {
	id := j.Request.ID
	if rec.LastMessage {
		if body := w.lastMessage(id, rec); body != "" {
			w.finish(j, node.JobCompleted, body, "")
			return
		}
	}
	body, err := s.answer()
	body = strings.TrimSpace(body)
	switch {
	case code > 0:
		errData, _ := os.ReadFile(w.runBase(id, rec.Attempt) + ".err")
		why := reason(string(errData))
		if s.failure != "" {
			why = s.failure
		}
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler failed: %s: exit status %d: %s", rec.Name, code, why))
	case err != nil:
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler failed: %v", err))
	case body == "":
		w.finish(j, node.JobFailed, "", "handler returned no text")
	default:
		w.finish(j, node.JobCompleted, body, "")
	}
}

// Cancel stops job id: a queued job fails at once, a running detached agent
// has its process tree killed and the job fails. It reports whether the job
// was queued or running.
func (w *Worker) Cancel(id string) bool {
	w.mu.Lock()
	j, ok := w.jobs[id]
	if !ok || j.terminal() {
		w.mu.Unlock()
		return false
	}
	if ch, running := w.live[id]; running {
		delete(w.live, id)
		close(ch)
		w.mu.Unlock()
		return true
	}
	queued := j.Status == node.JobQueued
	w.mu.Unlock()
	if queued {
		w.finish(j, node.JobFailed, "", ErrCancelled)
	}
	return queued
}

// CancelAll kills every running detached agent and fails its job: the
// "shut down and kill jobs" path. Stopping Run alone leaves agents running.
func (w *Worker) CancelAll() {
	w.mu.Lock()
	ids := make([]string, 0, len(w.live))
	for id := range w.live {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	for _, id := range ids {
		w.Cancel(id)
	}
}

// killLeftover kills a detached agent that no handler will watch.
func killLeftover(ctx context.Context, rec *Proc) {
	if rec == nil {
		return
	}
	if _, ok := attach(rec.PID, rec.Start); ok {
		_ = killTree(ctx, rec.PID)
	}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
