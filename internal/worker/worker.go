// Package worker answers inbound requests by running a local coding agent
// headless and read-only, one job at a time, and sending its final text back
// as a reply.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// DefaultTimeout bounds one agent run.
const DefaultTimeout = 10 * time.Minute

// queueSize bounds how many requests may wait behind the running one.
const queueSize = 64

// Runner runs an agent in dir with prompt and returns its final text.
type Runner func(ctx context.Context, dir, prompt string) (string, error)

// SendFunc sends a reply; it matches node.Node.Send.
type SendFunc func(to, body, replyTo string) (node.Message, error)

// Worker is a serial job queue in front of a Runner.
type Worker struct {
	run     Runner
	send    SendFunc
	dir     string
	timeout time.Duration
	log     *slog.Logger
	jobs    chan node.Message
}

// New creates a worker. log may be nil; timeout <= 0 means DefaultTimeout.
func New(run Runner, send SendFunc, dir string, timeout time.Duration, log *slog.Logger) *Worker {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Worker{run: run, send: send, dir: dir, timeout: timeout, log: log, jobs: make(chan node.Message, queueSize)}
}

// Offer queues m if it is a request (not a reply). It never blocks; a full
// queue answers the request with an error at once.
func (w *Worker) Offer(m node.Message) {
	if m.ReplyTo != "" {
		return
	}
	select {
	case w.jobs <- m:
	default:
		w.reply(m, "agentlink: handler queue is full, try again later")
	}
}

// Run processes queued jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-w.jobs:
			w.handle(ctx, m)
		}
	}
}

func (w *Worker) handle(ctx context.Context, m node.Message) {
	w.log.Info("handler started", "id", m.ID, "from", m.From)
	jobCtx, cancel := context.WithTimeout(ctx, w.timeout)
	out, err := w.run(jobCtx, w.dir, m.Body)
	jobErr := jobCtx.Err()
	cancel()
	body := strings.TrimSpace(out)
	switch {
	case errors.Is(jobErr, context.DeadlineExceeded):
		body = fmt.Sprintf("agentlink: handler timed out after %s", w.timeout)
	case ctx.Err() != nil:
		body = "agentlink: handler was interrupted (settings changed or the app quit)"
	case err != nil:
		body = fmt.Sprintf("agentlink: handler failed: %v", err)
	case body == "":
		body = "agentlink: handler returned no text"
	}
	w.log.Info("handler finished", "id", m.ID, "err", err)
	w.reply(m, body)
}

func (w *Worker) reply(m node.Message, body string) {
	if _, err := w.send(m.From, body, m.ID); err != nil {
		w.log.Error("send reply", "id", m.ID, "err", err)
	}
}
