// Package worker answers inbound requests by running a local coding agent
// headless and read-only, one job at a time, and sending its final text back
// as a reply.
//
// The job queue is durable: every request is recorded in <state dir>/jobs
// before the node ACKs it, and moves queued -> running -> completed | failed.
// A job interrupted while running (quit, crash, settings save) runs once more
// after restart; a second interruption fails it. Timeouts and agent errors fail
// at once. The sender gets status updates (queued, running) and a final reply
// that carries completed or failed.
package worker

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// DefaultTimeout bounds one agent run.
const DefaultTimeout = 10 * time.Minute

// MaxAttempts is how many times a job may start; only interruptions retry.
const MaxAttempts = 2

// Failure texts that tests and scripts match.
const (
	ErrNoHandler   = "no handler configured"
	ErrInterrupted = "handler was interrupted twice (the app quit, crashed or settings changed)"
)

// Runner runs an agent in dir with prompt and returns its final text.
type Runner func(ctx context.Context, dir, prompt string) (string, error)

// SendFunc queues a message; it matches node.Node.SendMessage.
type SendFunc func(m node.Message) (node.Message, error)

// Job is the durable state of one request.
type Job struct {
	Request    node.Message `json:"request"`
	Seq        int64        `json:"seq"` // acceptance order
	Status     string       `json:"status"`
	Attempts   int          `json:"attempts"`
	Error      string       `json:"error,omitempty"`
	Result     string       `json:"result,omitempty"`
	Replied    bool         `json:"replied"` // the final reply is in the outbox
	AcceptedAt time.Time    `json:"accepted_at"`
	StartedAt  time.Time    `json:"started_at,omitzero"`
	FinishedAt time.Time    `json:"finished_at,omitzero"`
}

func (j *Job) terminal() bool { return j.Status == node.JobCompleted || j.Status == node.JobFailed }

// Worker is a durable serial job queue in front of a Runner.
type Worker struct {
	run     Runner // nil: no handler, pending jobs fail
	send    SendFunc
	dir     string // agent working folder
	jobsDir string
	timeout time.Duration
	log     *slog.Logger
	kick    chan struct{}

	mu   sync.Mutex
	jobs map[string]*Job
	seq  int64
}

// New opens the job store in stateDir/jobs. run may be nil when no handler is
// configured. log may be nil; timeout <= 0 means DefaultTimeout.
func New(run Runner, send SendFunc, stateDir, dir string, timeout time.Duration, log *slog.Logger) (*Worker, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := &Worker{
		run: run, send: send, dir: dir, jobsDir: filepath.Join(stateDir, "jobs"), timeout: timeout, log: log,
		kick: make(chan struct{}, 1), jobs: map[string]*Job{},
	}
	if err := os.MkdirAll(w.jobsDir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(w.jobsDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(w.jobsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var j Job
		if err := json.Unmarshal(data, &j); err != nil {
			return nil, fmt.Errorf("job %s: %w", e.Name(), err)
		}
		w.jobs[j.Request.ID] = &j
		w.seq = max(w.seq, j.Seq)
	}
	return w, nil
}

// Accept durably queues m if it is a request and its id is new; replies,
// status updates and duplicates are ignored. It is the node's inbound hook:
// an error withholds the ACK so the sender resends.
func (w *Worker) Accept(m node.Message) error {
	if !m.IsRequest() {
		return nil
	}
	if _, err := hex.DecodeString(m.ID); err != nil || len(m.ID) != 32 {
		return fmt.Errorf("invalid request id %q", m.ID)
	}
	w.mu.Lock()
	if _, ok := w.jobs[m.ID]; ok {
		w.mu.Unlock()
		return nil
	}
	now := time.Now().UTC()
	j := &Job{Request: m, Seq: w.seq + 1, Status: node.JobQueued, AcceptedAt: now}
	if err := w.save(j); err != nil {
		w.mu.Unlock()
		return err
	}
	w.seq = j.Seq
	w.jobs[m.ID] = j
	w.mu.Unlock()
	w.log.Info("job queued", "id", m.ID, "from", m.From)
	w.status(m, node.JobQueued, "queued")
	select {
	case w.kick <- struct{}{}:
	default:
	}
	return nil
}

// Job returns a copy of the job for a request id.
func (w *Worker) Job(id string) (Job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j, ok := w.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Run recovers jobs left by a previous run, then processes queued jobs in
// acceptance order until ctx is cancelled. A job running at cancellation is
// left running on disk and retried by the next Run. Without a runner, Run
// fails the pending jobs and returns.
func (w *Worker) Run(ctx context.Context) {
	w.recover()
	if w.run == nil {
		for j := w.next(); j != nil; j = w.next() {
			w.finish(j, node.JobFailed, "", ErrNoHandler)
		}
		return
	}
	for ctx.Err() == nil {
		j := w.next()
		if j == nil {
			select {
			case <-ctx.Done():
			case <-w.kick:
			}
			continue
		}
		w.handle(ctx, j)
	}
}

// recover requeues or fails jobs found running and re-sends missing final replies.
func (w *Worker) recover() {
	w.mu.Lock()
	jobs := make([]*Job, 0, len(w.jobs))
	for _, j := range w.jobs {
		jobs = append(jobs, j)
	}
	w.mu.Unlock()
	slices.SortFunc(jobs, func(a, b *Job) int { return cmp.Compare(a.Seq, b.Seq) })
	for _, j := range jobs {
		switch {
		case j.Status == node.JobRunning && j.Attempts >= MaxAttempts:
			w.log.Warn("job interrupted again, failing", "id", j.Request.ID, "attempts", j.Attempts)
			w.finish(j, node.JobFailed, "", ErrInterrupted)
		case j.Status == node.JobRunning:
			w.log.Info("job interrupted, requeued", "id", j.Request.ID, "attempts", j.Attempts)
			w.mu.Lock()
			j.Status = node.JobQueued
			if err := w.save(j); err != nil {
				w.log.Error("save job", "id", j.Request.ID, "err", err)
			}
			w.mu.Unlock()
		case j.terminal() && !j.Replied:
			w.reply(j)
		}
	}
}

// next returns the oldest queued job.
func (w *Worker) next() *Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	var best *Job
	for _, j := range w.jobs {
		if j.Status == node.JobQueued && (best == nil || j.Seq < best.Seq) {
			best = j
		}
	}
	return best
}

func (w *Worker) handle(ctx context.Context, j *Job) {
	m := j.Request
	w.mu.Lock()
	j.Status, j.Attempts, j.StartedAt = node.JobRunning, j.Attempts+1, time.Now().UTC()
	err := w.save(j)
	if err != nil {
		j.Status, j.Attempts = node.JobQueued, j.Attempts-1
	}
	w.mu.Unlock()
	if err != nil {
		// Never run an attempt that is not on disk: it could repeat forever.
		w.log.Error("save job", "id", m.ID, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
		return
	}
	w.log.Info("handler started", "id", m.ID, "from", m.From, "attempt", j.Attempts)
	w.status(m, node.JobRunning, "running-"+strconv.Itoa(j.Attempts))

	jobCtx, cancel := context.WithTimeout(ctx, w.timeout)
	out, err := w.run(jobCtx, w.dir, m.Body)
	jobErr := jobCtx.Err()
	cancel()
	if ctx.Err() != nil && !errors.Is(jobErr, context.DeadlineExceeded) {
		w.log.Info("handler interrupted, will retry on restart", "id", m.ID, "attempt", j.Attempts)
		return
	}
	body := strings.TrimSpace(out)
	switch {
	case errors.Is(jobErr, context.DeadlineExceeded):
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler timed out after %s", w.timeout))
	case err != nil:
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler failed: %v", err))
	case body == "":
		w.finish(j, node.JobFailed, "", "handler returned no text")
	default:
		w.finish(j, node.JobCompleted, body, "")
	}
}

// finish records the outcome durably, then sends the final reply.
func (w *Worker) finish(j *Job, status, result, errText string) {
	w.mu.Lock()
	j.Status, j.Result, j.Error, j.FinishedAt = status, result, errText, time.Now().UTC()
	err := w.save(j)
	w.mu.Unlock()
	w.log.Info("handler finished", "id", j.Request.ID, "status", status, "error", errText)
	if err != nil {
		w.log.Error("save job", "id", j.Request.ID, "err", err)
	}
	w.reply(j)
}

// reply sends the final reply under an id derived from the request, so a
// reply re-sent after a crash is deduplicated by the receiver.
func (w *Worker) reply(j *Job) {
	w.mu.Lock()
	m, status, body := j.Request, j.Status, j.Result
	if status == node.JobFailed {
		body = "agentlink: " + j.Error
	}
	w.mu.Unlock()
	_, err := w.send(node.Message{
		ID: node.DerivedID(m.ID, "reply"), To: m.From, Body: body, ReplyTo: m.ID, JobStatus: status,
	})
	if err != nil {
		w.log.Error("send reply", "id", m.ID, "err", err)
		return
	}
	w.mu.Lock()
	j.Replied = true
	if err := w.save(j); err != nil {
		w.log.Error("save job", "id", m.ID, "err", err)
	}
	w.mu.Unlock()
}

func (w *Worker) status(m node.Message, status, label string) {
	_, err := w.send(node.Message{
		ID: node.DerivedID(m.ID, label), To: m.From, ReplyTo: m.ID, Kind: node.KindStatus, JobStatus: status,
	})
	if err != nil {
		w.log.Warn("send status", "id", m.ID, "status", status, "err", err)
	}
}

// save writes j atomically (temp file, fsync, rename). The caller holds w.mu.
func (w *Worker) save(j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(w.jobsDir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(w.jobsDir, j.Request.ID+".json"))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
