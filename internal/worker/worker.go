// Package worker answers inbound requests by running a local coding agent
// headless with full permissions, up to MaxJobs at a time, and sending its final text
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
// Timeouts (idle: no output from the agent, or the generous total cap), Cancel
// and agent errors fail at once and kill the agent's process tree. A request
// answered on this node another way (Answered) stops its job without a failure
// reply. The sender
// gets status updates (queued, running, then running with the agent's current
// activity, throttled) and a final reply that carries completed or failed.
// A chat request (Options.Chats) is answered to the whole chat and runs in the
// chat's own agent session (see sessions.go).
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
	"os/exec"
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
	// DefaultTimeout bounds one agent run in total; a long turn that keeps
	// working is stopped by it only as a last resort.
	DefaultTimeout = 60 * time.Minute
	// DefaultIdleTimeout fails a run whose agent printed nothing for this long.
	DefaultIdleTimeout = 10 * time.Minute
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
	// ErrAnswered notes a job stopped because this node answered the request
	// another way (see Answered); no failure is reported for it.
	ErrAnswered = "answered on this node"
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
	Agent func() Command
	// Project resolves a request's Area to the project directory it runs in.
	// An empty dir (no project for that area) keeps the work directory.
	Project         func(area string) (dir string)
	Timeout         time.Duration
	IdleTimeout     time.Duration
	MaxJobs         int
	ActivityEvery   time.Duration
	ActivityRefresh time.Duration
	// OnChange observes durable job state changes. It must return promptly.
	OnChange func()
	// Chats, when set, lets the worker answer chat requests (see sessions.go);
	// Self is this node's name, which chat reply and status ids include.
	Chats Chats
	Self  string
	// API is the node's local control API address, passed to the agent as
	// $AGENTLINK_API so its agentlink CLI works without --config.
	API string
	// ProjectID is the project of a project context's worker, passed to the
	// agent as $AGENTLINK_PROJECT_ID so its agentlink CLI picks that context.
	ProjectID string
	// Slots, when set, is the pool of job slots shared with the app's other
	// workers (see Slots); nil gives the worker its own open pool of MaxJobs.
	Slots *Slots
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
	// InputThrough is the last chat message (node's Seq) a chat job's run was
	// given; the chat's session is advanced to it when the job completes.
	InputThrough uint64 `json:"input_through,omitempty"`
	// Answered: this node answered the request another way (see Answered);
	// the job stops, completes without a reply and sends a neutral status.
	Answered bool `json:"answered,omitempty"`
}

func (j *Job) terminal() bool { return j.Status == node.JobCompleted || j.Status == node.JobFailed }

// Worker is a durable job queue in front of a Runner with MaxJobs slots.
type Worker struct {
	run         Runner // nil: no handler, pending jobs fail
	send        SendFunc
	dir         string // agent working folder
	jobsDir     string
	sessionsDir string
	opt         Options
	log         *slog.Logger
	kick        chan struct{}

	slots *Slots

	mu   sync.Mutex
	jobs map[string]*Job
	seq  int64
	// reattach holds running jobs left by a previous app, claimed before
	// queued ones, each with the slot Reattach holds for it.
	reattach map[string]func()
	// recovered: Reattach ran (once per Worker).
	recovered bool
	// draining: Quiesce closed the intake; Accept refuses requests (errDraining).
	draining bool
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
		run: run, send: send, dir: dir, jobsDir: filepath.Join(stateDir, "jobs"), sessionsDir: filepath.Join(stateDir, "sessions"),
		opt: opt.withDefaults(), log: log,
		kick: make(chan struct{}, 1), jobs: map[string]*Job{}, reattach: map[string]func(){}, live: map[string]chan struct{}{},
		slots: opt.Slots,
	}
	if w.slots == nil {
		w.slots = NewSlots(w.opt.MaxJobs)
		w.slots.Open()
	}
	for _, d := range []string{w.jobsDir, w.sessionsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
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
// status updates and duplicates are ignored. A request for an area with a live
// session (Chats.LiveSession) is left to that session, unread. A chat message
// is a request only when the node assigns it to this worker (Chats.ClaimRun,
// atomic with a session's ack). A chat message that asks this node but will
// not run (no agent program, the chain limit, a closed chat) gets a JobHeld
// status to the chat instead, so its sender never waits in silence; without a
// handler (auto-answer off) only the chain limit does, the rest waits unread. It is the node's inbound hook (only
// for chat messages when no handler is configured: see ChatsOnly); an error
// withholds the ACK so the sender resends.
func (w *Worker) Accept(m node.Message) error {
	chat := m.ChatID != "" && m.Kind == ""
	if chat && (w.opt.Chats == nil || !m.Asks(w.opt.Self)) || !chat && !m.IsRequest() {
		return nil
	}
	if _, err := hex.DecodeString(m.ID); err != nil || len(m.ID) != 32 {
		return fmt.Errorf("invalid request id %q", m.ID)
	}
	w.mu.Lock()
	_, known := w.jobs[m.ID]
	draining := w.draining
	w.mu.Unlock()
	if known {
		return nil
	}
	if draining {
		return errDraining // not ACKed: the sender resends it to where the project goes next
	}
	if !chat && w.opt.Chats != nil && w.opt.Chats.LiveSession(m.Area) {
		return nil // a live session there reads it (unread)
	}
	if chat {
		if !w.hasHandler() {
			// Auto-answer is off: the message waits unread for a session there.
			return nil
		}
		if w.opt.Chats.AutoHeld(m) {
			return nil // stop the automatic chain without a visible hold
		}
		if w.opt.Chats.LiveSession(m.Area) {
			return nil // the live session answers it
		}
		if hold := w.unavailable(); hold != "" {
			w.hold(m, hold)
			return nil
		}
		ok, hold, err := w.opt.Chats.ClaimRun(m)
		if err != nil {
			return err
		}
		if !ok {
			if hold != "" && hold != node.HoldAutoLimit {
				w.hold(m, hold)
			}
			return nil
		}
	}
	w.mu.Lock()
	if _, ok := w.jobs[m.ID]; ok {
		w.mu.Unlock()
		return nil
	}
	if w.draining {
		w.mu.Unlock()
		return errDraining
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
	w.log.Info("job queued", "id", m.ID, "from", m.From, "chat", m.ChatID)
	w.status(m, node.JobQueued, "queued", nil)
	w.wake()
	return nil
}

// ChatsOnly is the inbound hook of a node without a handler: chat requests
// get their held status from Accept, other requests stay for a person.
func (w *Worker) ChatsOnly(m node.Message) error {
	if m.ChatID == "" {
		return nil
	}
	return w.Accept(m)
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
	w.Reattach(ctx)
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
	// Reattached jobs no slot got to stay running on disk for the next start;
	// the shared slots they held are free again.
	w.mu.Lock()
	for id, release := range w.reattach {
		release()
		delete(w.reattach, id)
	}
	w.mu.Unlock()
}

// Reattach recovers the jobs a previous app left (see recover) and holds a
// slot (Slots.Hold) for every job still running, so the shared slots count
// them before any new job starts. The app runs it for every worker before it
// opens the shared slots; Run runs it too, and only the first call acts.
func (w *Worker) Reattach(ctx context.Context) {
	w.mu.Lock()
	done := w.recovered
	w.recovered = true
	w.mu.Unlock()
	if !done {
		w.recover(ctx)
	}
}

// slot runs jobs one after another until ctx is cancelled: a reattached job
// in the slot Reattach holds for it, a new one in a slot of the shared pool.
func (w *Worker) slot(ctx context.Context) {
	for ctx.Err() == nil {
		if j, release := w.claimReattach(); j != nil {
			w.runJob(ctx, j, true)
			release()
			continue
		}
		if w.next() == nil {
			select {
			case <-ctx.Done():
			case <-w.kick:
			}
			continue
		}
		release, ok := w.slots.Acquire(ctx)
		if !ok {
			return
		}
		j, err := w.claim()
		switch {
		case err != nil:
			release()
			// Never run an attempt that is not on disk: it could repeat forever.
			w.log.Error("save job", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		case j == nil: // another slot took it
			release()
		default:
			w.runJob(ctx, j, false)
			release()
		}
	}
}

// runJob runs claimed job j (reattach: a running one left by a previous app).
func (w *Worker) runJob(ctx context.Context, j *Job, reattach bool) {
	switch {
	case !reattach && w.chatClosed(j.Request):
		// Queued before the close arrived: no new work in a closed chat.
		w.finish(j, node.JobFailed, "", ErrChatClosed)
	case w.opt.Agent != nil:
		w.wake()
		w.handleDetached(ctx, j, reattach)
	default:
		w.wake()
		w.handle(ctx, j)
	}
}

// ErrBusy: the worker has unfinished jobs (Quiesce).
var ErrBusy = errors.New("worker has unfinished jobs")

// errDraining: Quiesce closed the intake; the request is not accepted (not
// ACKed) and its sender resends it later.
var errDraining = errors.New("worker is draining")

// Busy reports whether any job is not finished (queued or running).
func (w *Worker) Busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busyLocked()
}

func (w *Worker) busyLocked() bool {
	for _, j := range w.jobs {
		if !j.terminal() {
			return true
		}
	}
	return false
}

// Quiesce closes the intake for a leave or a folder change, atomically with
// the check for unfinished jobs: with any, the intake stays open and it
// returns ErrBusy; otherwise Accept refuses every request from now on
// (errDraining, so it is not ACKed) until Resume.
func (w *Worker) Quiesce() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busyLocked() {
		return ErrBusy
	}
	w.draining = true
	return nil
}

// Resume opens the intake Quiesce closed (the change it was for failed).
func (w *Worker) Resume() {
	w.mu.Lock()
	w.draining = false
	w.mu.Unlock()
}

func (w *Worker) hasHandler() bool { return w.run != nil || w.opt.Agent != nil }

// unavailable reports why no job could answer a request now: no handler, or
// the agent program is not found (after the handler looked for it again).
func (w *Worker) unavailable() string {
	switch {
	case !w.hasHandler():
		return node.HoldNoHandler
	case w.run == nil:
		if _, err := exec.LookPath(w.opt.Agent().Name); err != nil {
			return node.HoldNoAgent
		}
	}
	return ""
}

// hold tells chat request m's chat that this node will not answer it
// automatically, and why: a terminal JobHeld status.
func (w *Worker) hold(m node.Message, reason string) {
	w.log.Warn("chat request held", "id", m.ID, "from", m.From, "chat", m.ChatID, "reason", reason)
	out := node.Message{ReplyTo: m.ID, Kind: node.KindStatus, JobStatus: node.JobHeld, HoldReason: reason, Activity: node.HoldText(reason)}
	w.address(&out, m, "held")
	if _, err := w.send(out); err != nil {
		w.log.Warn("send status", "id", m.ID, "status", node.JobHeld, "err", err)
	}
}

// chatClosed reports whether m belongs to a chat that is closed or gone.
func (w *Worker) chatClosed(m node.Message) bool {
	if m.ChatID == "" || w.opt.Chats == nil {
		return false
	}
	c, ok := w.opt.Chats.ChatOf(m.ChatID)
	return !ok || c.Closed()
}

// where resolves the directory a request runs in: the project mapped to its
// area, else the work directory.
func (w *Worker) where(area string) string {
	if w.opt.Project != nil && area != "" {
		if dir := w.opt.Project(area); dir != "" {
			return dir
		}
	}
	return w.dir
}

// claimReattach returns the oldest running job left by a previous app, with
// the release of the slot Reattach holds for it; nil when there is none.
func (w *Worker) claimReattach() (*Job, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var j *Job
	for id := range w.reattach {
		if c := w.jobs[id]; j == nil || c.Seq < j.Seq {
			j = c
		}
	}
	if j == nil {
		return nil, nil
	}
	release := w.reattach[j.Request.ID]
	delete(w.reattach, j.Request.ID)
	return j, release
}

// claim durably moves the oldest queued job to running and returns it; nil
// when there is none.
func (w *Worker) claim() (*Job, error) {
	w.mu.Lock()
	j := w.nextLocked()
	if j == nil {
		w.mu.Unlock()
		return nil, nil
	}
	j.Status, j.Attempts, j.StartedAt, j.Proc = node.JobRunning, j.Attempts+1, time.Now().UTC(), nil
	if err := w.save(j); err != nil {
		j.Status, j.Attempts = node.JobQueued, j.Attempts-1
		w.mu.Unlock()
		return nil, err
	}
	w.mu.Unlock()
	w.changed()
	return j, nil
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
			// A detached agent: reattach, finalize or resume it in a slot,
			// which it holds from now on (it is running).
			w.mu.Lock()
			w.reattach[j.Request.ID] = w.slots.Hold()
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

// nextLocked skips the jobs of a chat that already has one running (a
// reattached one too): a chat's session takes one turn at a time.
func (w *Worker) nextLocked() *Job {
	busy := map[string]bool{}
	for _, j := range w.jobs {
		if j.Status == node.JobRunning && j.Request.ChatID != "" {
			busy[j.Request.ChatID] = true
		}
	}
	var best *Job
	for _, j := range w.jobs {
		if j.Status == node.JobQueued && !j.Answered && !busy[j.Request.ChatID] && (best == nil || j.Seq < best.Seq) {
			best = j
		}
	}
	return best
}

// handle runs a claimed job. The run is cancelled (its process tree killed)
// after Timeout, or after IdleTimeout without any output from the agent.
func (w *Worker) handle(ctx context.Context, j *Job) {
	m := j.Request
	cancelled := make(chan struct{})
	w.mu.Lock()
	attempt := j.Attempts
	w.live[m.ID] = cancelled
	if j.Answered { // answered while it was being claimed
		delete(w.live, m.ID)
		close(cancelled)
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.live, m.ID)
		w.mu.Unlock()
	}()
	w.log.Info("handler started", "id", m.ID, "from", m.From, "attempt", attempt)
	w.status(m, node.JobRunning, "running-"+strconv.Itoa(attempt), nil)
	prompt := m.Body
	if m.ChatID != "" && w.opt.Chats != nil {
		// A Runner keeps no session: every run gets the latest chat messages.
		prompt, _ = w.chatInput(m, 0, false)
	}

	idleCtx, cancelIdle := context.WithCancelCause(ctx)
	jobCtx, cancel := context.WithTimeoutCause(idleCtx, w.opt.Timeout, errHardTimeout)
	idle := time.AfterFunc(w.opt.IdleTimeout, func() { cancelIdle(errIdleTimeout) })
	go func() {
		select {
		case <-cancelled:
			cancelIdle(errCancelled)
		case <-idleCtx.Done():
		}
	}()
	relay := w.relay(m, "activity-"+strconv.Itoa(attempt))
	out, err := w.run(jobCtx, w.where(m.Area), prompt, func(activity string) {
		idle.Reset(w.opt.IdleTimeout)
		relay.set(node.ActivityState{Type: TypeTool, Text: activity, Phase: node.PhaseRunning})
	})
	idle.Stop()
	relay.stop()
	cause := context.Cause(jobCtx)
	cancel()
	cancelIdle(nil)
	body := strings.TrimSpace(out)
	done := err == nil && body != "" // a timer that fired after the run ended does not count
	if !done && ctx.Err() != nil && !errors.Is(cause, errHardTimeout) && !errors.Is(cause, errIdleTimeout) && !errors.Is(cause, errCancelled) {
		w.log.Info("handler interrupted, will retry on restart", "id", m.ID, "attempt", attempt)
		return
	}
	switch {
	case done:
		w.finish(j, node.JobCompleted, body, "")
	case errors.Is(cause, errCancelled):
		w.finish(j, node.JobFailed, "", ErrCancelled)
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
//
// A refresh re-sends the same activity (same id and start time, a new Seq),
// so the receiver's elapsed time keeps counting from the operation's start.
type relay struct {
	mu      sync.Mutex
	current node.ActivityState
	changed chan struct{}
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func sameActivity(a, b node.ActivityState) bool {
	return a.ID == b.ID && a.Text == b.Text && a.Phase == b.Phase && a.Type == b.Type
}

// relay starts one; update ids are prefix-1, prefix-2, ...
func (w *Worker) relay(m node.Message, prefix string) *relay {
	r := &relay{changed: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		refresh := time.NewTimer(w.opt.ActivityRefresh)
		defer refresh.Stop()
		var sent node.ActivityState
		n := 0
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
			if cur.Text == "" || (sameActivity(cur, sent) && time.Since(sentAt) < w.opt.ActivityRefresh) {
				continue
			}
			n++
			// Seq orders updates across restarts of the app too.
			cur.Seq = uint64(time.Now().UnixNano())
			w.status(m, node.JobRunning, prefix+"-"+strconv.Itoa(n), &cur)
			sent, sentAt = cur, time.Now()
			refresh.Reset(w.opt.ActivityRefresh)
		}
	}()
	return r
}

// set records the latest activity; one without Text keeps the previous one.
// An activity without a start time starts when it first shows up.
func (r *relay) set(a node.ActivityState) {
	if a.Text == "" {
		return
	}
	r.mu.Lock()
	changed := !sameActivity(a, r.current)
	if a.StartedAt.IsZero() {
		a.StartedAt = r.current.StartedAt
		if changed {
			a.StartedAt = time.Now().UTC()
		}
	}
	r.current = a
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
// A job answered on this node another way completes without a result.
func (w *Worker) finish(j *Job, status, result, errText string) {
	w.mu.Lock()
	answered := j.Answered
	if answered {
		status, result, errText = node.JobCompleted, "", ErrAnswered
	}
	j.Status, j.Result, j.Error, j.FinishedAt = status, result, errText, time.Now().UTC()
	err := w.save(j)
	w.mu.Unlock()
	if err == nil {
		w.changed()
	}
	w.log.Info("handler finished", "id", j.Request.ID, "status", status, "error", errText)
	if err != nil {
		w.log.Error("save job", "id", j.Request.ID, "err", err)
	} else if status == node.JobCompleted && !answered {
		// An answered job's session did not take the turn, so it is not advanced.
		w.advanceSession(j)
		// A failed run keeps its files (<jobs>/<id>/) for diagnosis.
		_ = os.RemoveAll(filepath.Join(w.jobsDir, j.Request.ID))
	}
	w.reply(j)
	if j.Request.ChatID != "" {
		w.wake() // the chat's next request may run now
	}
}

// reply sends the final reply under an id derived from the request, so a
// reply re-sent after a crash is deduplicated by the receiver. A job answered
// on this node another way sends only a completed status: the answer is out.
func (w *Worker) reply(j *Job) {
	w.mu.Lock()
	m, status, body := j.Request, j.Status, j.Result
	if status == node.JobFailed {
		body = "agentlink: " + j.Error
	}
	answered := j.Answered
	w.mu.Unlock()
	out := node.Message{Body: body, ReplyTo: m.ID, JobStatus: status, AuthorKind: node.AuthorWorker}
	w.address(&out, m, "reply")
	if answered {
		out = node.Message{ReplyTo: m.ID, Kind: node.KindStatus, JobStatus: node.JobCompleted}
		if m.ChatID != "" {
			out.HoldReason = node.HoldAnswered
		}
		w.address(&out, m, "answered")
	}
	_, err := w.send(out)
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

// status sends a status update of request m; a carries the agent's activity.
func (w *Worker) status(m node.Message, status, label string, a *node.ActivityState) {
	out := node.Message{ReplyTo: m.ID, Kind: node.KindStatus, JobStatus: status}
	w.address(&out, m, label)
	if a != nil {
		out.Activity = a.Text
		if m.ChatID != "" {
			info := *a
			out.ActivityInfo = &info
		}
	}
	if _, err := w.send(out); err != nil {
		w.log.Warn("send status", "id", m.ID, "status", status, "err", err)
	}
}

// address sends out back to where request m came from under an id derived
// from m and label: to its sender, or to the whole chat. In a chat several
// participants answer the same request, so the id includes this node.
func (w *Worker) address(out *node.Message, m node.Message, label string) {
	if m.ChatID == "" {
		out.ID, out.To = node.DerivedID(m.ID, label), m.From
		return
	}
	out.ID, out.ChatID = node.DerivedID(m.ID, w.opt.Self+"/"+label), m.ChatID
}

// save writes j atomically. The caller holds w.mu.
func (w *Worker) save(j *Job) error { return writeAtomic(w.jobsDir, j.Request.ID+".json", j) }
