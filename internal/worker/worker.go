// Package worker answers inbound requests by running a local coding agent
// headless and read-only, up to MaxJobs at a time, and sending its final text
// back as a reply.
//
// The job queue is durable: every request is recorded in <state dir>/jobs
// before the node ACKs it, and moves queued -> running -> completed | failed.
// Jobs start in arrival order; with several slots they may finish in any
// order. With Options.Agent the agent runs detached (see Proc): quitting,
// restarting or updating the app leaves it running and the next start
// reattaches to it, or finalizes it from its output, or resumes its session
// when it died mid-run. A job interrupted while running without a way to
// continue runs once more after restart; a second interruption fails it.
// Timeouts (hard, or idle: no output from the agent), Cancel and agent errors
// fail at once and kill the agent's process tree. The sender
// gets status updates (queued, running, then running with the agent's current
// activity, throttled) and a final reply that carries completed or failed.
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

// Defaults of Options.
const (
	// DefaultTimeout bounds one agent run.
	DefaultTimeout = 10 * time.Minute
	// DefaultIdleTimeout fails a run whose agent printed nothing for this long.
	DefaultIdleTimeout = 3 * time.Minute
	// DefaultMaxJobs is how many agents run at once.
	DefaultMaxJobs = 2
	// MaxMaxJobs is the largest MaxJobs the settings accept.
	MaxMaxJobs = 4
	// DefaultActivityEvery is the least time between two activity updates.
	DefaultActivityEvery = 3 * time.Second
	// DefaultActivityRefresh re-sends an unchanged activity this often, so the
	// sender keeps hearing about a job that is still running.
	DefaultActivityRefresh = 2 * time.Minute
)

// MaxAttempts is how many times a job may start; only interruptions retry.
const MaxAttempts = 2

// Failure texts that tests and scripts match.
const (
	ErrNoHandler   = "no handler configured"
	ErrInterrupted = "handler was interrupted twice (the agent or the app stopped mid-run)"
	// ErrIdle starts the failure of a run stopped by the idle timeout.
	ErrIdle = "агент завис"
)

var (
	errHardTimeout = errors.New("hard timeout")
	errIdleTimeout = errors.New("idle timeout")
)

// Runner runs an agent in dir with prompt and returns its final text. It
// calls progress (never concurrently) for everything the agent prints: with
// the activity it describes, or "" when it only shows the agent is alive.
type Runner func(ctx context.Context, dir, prompt string, progress func(activity string)) (string, error)

// SendFunc queues a message; it matches node.Node.SendMessage.
type SendFunc func(m node.Message) (node.Message, error)

// Options tune a Worker; zero fields take the defaults.
type Options struct {
	// Agent, when set, returns the agent command, which then runs detached
	// (see Proc) and survives a restart of this app; the Runner is not used.
	Agent           func() Command
	Timeout         time.Duration
	IdleTimeout     time.Duration
	MaxJobs         int
	ActivityEvery   time.Duration
	ActivityRefresh time.Duration
	// OnChange observes durable job state changes. It must return promptly.
	OnChange func()
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.MaxJobs <= 0 {
		o.MaxJobs = DefaultMaxJobs
	}
	if o.ActivityEvery <= 0 {
		o.ActivityEvery = DefaultActivityEvery
	}
	if o.ActivityRefresh <= 0 {
		o.ActivityRefresh = DefaultActivityRefresh
	}
	return o
}

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
	// Proc is the detached agent run of the current attempt, if any.
	Proc *Proc `json:"proc,omitempty"`
}

func (j *Job) terminal() bool { return j.Status == node.JobCompleted || j.Status == node.JobFailed }

// Worker is a durable job queue in front of a Runner with MaxJobs slots.
type Worker struct {
	run     Runner // nil: no handler, pending jobs fail
	send    SendFunc
	dir     string // agent working folder
	jobsDir string
	opt     Options
	log     *slog.Logger
	kick    chan struct{}

	mu   sync.Mutex
	jobs map[string]*Job
	seq  int64
	// reattach holds running jobs left by a previous app, claimed before queued ones.
	reattach map[string]bool
	// live maps a watched detached job to its cancel signal.
	live map[string]chan struct{}
}

// New opens the job store in stateDir/jobs. run may be nil when no handler is
// configured. log may be nil.
func New(run Runner, send SendFunc, stateDir, dir string, opt Options, log *slog.Logger) (*Worker, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := &Worker{
		run: run, send: send, dir: dir, jobsDir: filepath.Join(stateDir, "jobs"), opt: opt.withDefaults(), log: log,
		kick: make(chan struct{}, 1), jobs: map[string]*Job{}, reattach: map[string]bool{}, live: map[string]chan struct{}{},
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
	w.changed()
	w.log.Info("job queued", "id", m.ID, "from", m.From)
	w.status(m, node.JobQueued, "queued", "")
	w.wake()
	return nil
}

// wake lets one idle slot look for a queued job. A slot that takes a job
// wakes the next one, so a burst of requests fills every free slot.
func (w *Worker) wake() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Worker) changed() {
	if w.opt.OnChange != nil {
		w.opt.OnChange()
	}
}

// Job returns a copy of the job for a request id.
func (w *Worker) Job(id string) (Job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j, ok := w.jobs[id]
	if !ok {
		return Job{}, false
	}
	c := *j
	if j.Proc != nil {
		p := *j.Proc
		c.Proc = &p
	}
	return c, true
}

// Run recovers jobs left by a previous run, then runs queued jobs, starting
// them in acceptance order on MaxJobs slots, until ctx is cancelled; it
// returns when every slot has stopped. A job running at cancellation is left
// running on disk and retried by the next Run. Without a runner, Run fails
// the pending jobs and returns.
func (w *Worker) Run(ctx context.Context) {
	w.recover(ctx)
	if !w.hasHandler() {
		for j := w.next(); j != nil; j = w.next() {
			w.finish(j, node.JobFailed, "", ErrNoHandler)
		}
		return
	}
	var slots sync.WaitGroup
	for range w.opt.MaxJobs {
		slots.Go(func() { w.slot(ctx) })
	}
	slots.Wait()
}

// slot runs jobs one after another until ctx is cancelled.
func (w *Worker) slot(ctx context.Context) {
	for ctx.Err() == nil {
		j, reattach, err := w.claim()
		switch {
		case err != nil:
			// Never run an attempt that is not on disk: it could repeat forever.
			w.log.Error("save job", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		case j == nil:
			select {
			case <-ctx.Done():
			case <-w.kick:
			}
		case w.opt.Agent != nil:
			w.wake()
			w.handleDetached(ctx, j, reattach)
		default:
			w.wake()
			w.handle(ctx, j)
		}
	}
}

func (w *Worker) hasHandler() bool { return w.run != nil || w.opt.Agent != nil }

// claim returns a running job left by a previous app (reattach), else durably
// moves the oldest queued job to running and returns it; nil when neither.
func (w *Worker) claim() (j *Job, reattach bool, err error) {
	w.mu.Lock()
	for id := range w.reattach {
		if c := w.jobs[id]; j == nil || c.Seq < j.Seq {
			j = c
		}
	}
	if j != nil {
		delete(w.reattach, j.Request.ID)
		w.mu.Unlock()
		return j, true, nil
	}
	j = w.nextLocked()
	if j == nil {
		w.mu.Unlock()
		return nil, false, nil
	}
	j.Status, j.Attempts, j.StartedAt, j.Proc = node.JobRunning, j.Attempts+1, time.Now().UTC(), nil
	if err := w.save(j); err != nil {
		j.Status, j.Attempts = node.JobQueued, j.Attempts-1
		w.mu.Unlock()
		return nil, false, err
	}
	w.mu.Unlock()
	w.changed()
	return j, false, nil
}

// recover requeues or fails jobs found running and re-sends missing final replies.
func (w *Worker) recover(ctx context.Context) {
	w.mu.Lock()
	jobs := make([]*Job, 0, len(w.jobs))
	for _, j := range w.jobs {
		jobs = append(jobs, j)
	}
	w.mu.Unlock()
	slices.SortFunc(jobs, func(a, b *Job) int { return cmp.Compare(a.Seq, b.Seq) })
	for _, j := range jobs {
		switch {
		case j.Status == node.JobRunning && j.Proc != nil && w.opt.Agent != nil:
			// A detached agent: reattach, finalize or resume it in a slot.
			w.mu.Lock()
			w.reattach[j.Request.ID] = true
			w.mu.Unlock()
			continue
		case j.Status == node.JobRunning && j.Proc != nil:
			// No handler to watch it any more.
			killLeftover(ctx, j.Proc)
		}
		switch {
		case j.Status == node.JobRunning && j.Attempts >= MaxAttempts:
			w.log.Warn("job interrupted again, failing", "id", j.Request.ID, "attempts", j.Attempts)
			w.finish(j, node.JobFailed, "", ErrInterrupted)
		case j.Status == node.JobRunning:
			w.log.Info("job interrupted, requeued", "id", j.Request.ID, "attempts", j.Attempts)
			w.mu.Lock()
			j.Status = node.JobQueued
			err := w.save(j)
			stored := err == nil
			if err != nil {
				w.log.Error("save job", "id", j.Request.ID, "err", err)
			}
			w.mu.Unlock()
			if stored {
				w.changed()
			}
		case j.terminal() && !j.Replied:
			w.reply(j)
		}
	}
}

// next returns the oldest queued job.
func (w *Worker) next() *Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextLocked()
}

func (w *Worker) nextLocked() *Job {
	var best *Job
	for _, j := range w.jobs {
		if j.Status == node.JobQueued && (best == nil || j.Seq < best.Seq) {
			best = j
		}
	}
	return best
}

// handle runs a claimed job. The run is cancelled (its process tree killed)
// after Timeout, or after IdleTimeout without any output from the agent.
func (w *Worker) handle(ctx context.Context, j *Job) {
	m := j.Request
	w.mu.Lock()
	attempt := j.Attempts
	w.mu.Unlock()
	w.log.Info("handler started", "id", m.ID, "from", m.From, "attempt", attempt)
	w.status(m, node.JobRunning, "running-"+strconv.Itoa(attempt), "")

	idleCtx, cancelIdle := context.WithCancelCause(ctx)
	jobCtx, cancel := context.WithTimeoutCause(idleCtx, w.opt.Timeout, errHardTimeout)
	idle := time.AfterFunc(w.opt.IdleTimeout, func() { cancelIdle(errIdleTimeout) })
	relay := w.relay(m, "activity-"+strconv.Itoa(attempt))
	out, err := w.run(jobCtx, w.dir, m.Body, func(activity string) {
		idle.Reset(w.opt.IdleTimeout)
		relay.set(activity)
	})
	idle.Stop()
	relay.stop()
	cause := context.Cause(jobCtx)
	cancel()
	cancelIdle(nil)
	body := strings.TrimSpace(out)
	done := err == nil && body != "" // a timer that fired after the run ended does not count
	if !done && ctx.Err() != nil && !errors.Is(cause, errHardTimeout) && !errors.Is(cause, errIdleTimeout) {
		w.log.Info("handler interrupted, will retry on restart", "id", m.ID, "attempt", attempt)
		return
	}
	switch {
	case done:
		w.finish(j, node.JobCompleted, body, "")
	case errors.Is(cause, errIdleTimeout):
		w.finish(j, node.JobFailed, "", fmt.Sprintf("%s (нет активности %s)", ErrIdle, minutes(w.opt.IdleTimeout)))
	case errors.Is(cause, errHardTimeout):
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler timed out after %s", w.opt.Timeout))
	case err != nil:
		w.finish(j, node.JobFailed, "", fmt.Sprintf("handler failed: %v", err))
	default:
		w.finish(j, node.JobFailed, "", "handler returned no text")
	}
}

// minutes renders d as «3 мин» when it is whole minutes, else as a Go duration.
func minutes(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + " мин"
	}
	return d.String()
}

// relay forwards a running job's activity to the sender as status updates:
// the first one at once, then at most one per ActivityEvery and only when the
// activity changed, plus the unchanged one again after ActivityRefresh.
type relay struct {
	mu      sync.Mutex
	current string
	changed chan struct{}
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// relay starts one; update ids are prefix-1, prefix-2, ...
func (w *Worker) relay(m node.Message, prefix string) *relay {
	r := &relay{changed: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		refresh := time.NewTimer(w.opt.ActivityRefresh)
		defer refresh.Stop()
		sent, n := "", 0
		var sentAt time.Time
		for {
			select {
			case <-r.quit:
				return
			case <-r.changed:
			case <-refresh.C:
			}
			if wait := w.opt.ActivityEvery - time.Since(sentAt); wait > 0 {
				select {
				case <-r.quit:
					return
				case <-time.After(wait):
				}
			}
			r.mu.Lock()
			cur := r.current
			r.mu.Unlock()
			if cur == "" || (cur == sent && time.Since(sentAt) < w.opt.ActivityRefresh) {
				continue
			}
			n++
			w.status(m, node.JobRunning, prefix+"-"+strconv.Itoa(n), cur)
			sent, sentAt = cur, time.Now()
			refresh.Reset(w.opt.ActivityRefresh)
		}
	}()
	return r
}

// set records the latest activity; "" keeps the previous one.
func (r *relay) set(activity string) {
	if activity == "" {
		return
	}
	r.mu.Lock()
	changed := activity != r.current
	r.current = activity
	r.mu.Unlock()
	if changed {
		select {
		case r.changed <- struct{}{}:
		default:
		}
	}
}

// stop ends the relay; no update is sent after it returns. It may be called again.
func (r *relay) stop() {
	r.once.Do(func() { close(r.quit) })
	<-r.done
}

// finish records the outcome durably, then sends the final reply.
func (w *Worker) finish(j *Job, status, result, errText string) {
	w.mu.Lock()
	j.Status, j.Result, j.Error, j.FinishedAt = status, result, errText, time.Now().UTC()
	err := w.save(j)
	w.mu.Unlock()
	if err == nil {
		w.changed()
	}
	w.log.Info("handler finished", "id", j.Request.ID, "status", status, "error", errText)
	if err != nil {
		w.log.Error("save job", "id", j.Request.ID, "err", err)
	} else if status == node.JobCompleted {
		// A failed run keeps its files (<jobs>/<id>/) for diagnosis.
		_ = os.RemoveAll(filepath.Join(w.jobsDir, j.Request.ID))
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
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.changed()
}

func (w *Worker) status(m node.Message, status, label, activity string) {
	_, err := w.send(node.Message{
		ID: node.DerivedID(m.ID, label), To: m.From, ReplyTo: m.ID, Kind: node.KindStatus, JobStatus: status,
		Activity: activity,
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
