package node

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// What the node does for a session whose client delivers in-process (Claude
// Code's hooks module, SessionRequest.Native) instead of through shell hooks:
// it holds the module's unread query until there is something (WaitUnread),
// takes back what a turn that never ran had claimed (Release), and keeps the
// shell hooks of the same session out (sessionNative).

// nativeSeenTTL is how long a module that says nothing still has its session:
// three of its heartbeats (HEARTBEAT_MS of plugins/agent-link/hooks/register.ts).
const nativeSeenTTL = 3 * time.Minute

// sessionNative reports whether live session sid is delivered to by its
// in-process module: its shell hooks and their waiter get nothing then.
//
// Only while the module is heard (Session.NativeSeen, which the shell hooks'
// registrations never move): one silent for nativeSeenTTL is gone (refused at
// a reload, disabled, crashed) though the session's shell hooks still run, and
// the session is theirs again. What the module had claimed for a turn and
// never acknowledged is released then (a wake it submitted stays the wake's:
// the hooks acknowledge it at the prompt that carries its token, or deliver
// it once it lapsed, as they do for the node's own wakes).
func (n *Node) sessionNative(sid string) bool {
	if sid == "" {
		return false
	}
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	s := r.sessions[sid]
	if s == nil || !s.Native || !s.live(now) {
		r.mu.Unlock()
		return false
	}
	if now.Sub(s.NativeSeen) < nativeSeenTTL {
		r.mu.Unlock()
		return true
	}
	r.mu.Unlock()
	// The module's claims go before the session is its hooks' again, both
	// under claimMu: a hook's claim follows the lapse and is never among them.
	r.claimMu.Lock()
	r.mu.Lock()
	s = r.sessions[sid]
	if s == nil || !s.Native || now.Sub(s.NativeSeen) < nativeSeenTTL {
		// Lapsed already by another caller, or the module was heard meanwhile.
		still := s != nil && s.Native && s.live(now)
		r.mu.Unlock()
		r.claimMu.Unlock()
		return still
	}
	for id, c := range r.claims {
		if c.owner == sid && c.kind == holdHook {
			delete(r.claims, id)
		}
	}
	s.Native = false
	err := r.saveLocked(now)
	r.mu.Unlock()
	r.claimMu.Unlock()
	if err != nil {
		n.log.Warn("save sessions", "err", err)
	}
	n.log.Warn("the session's in-process module fell silent; its shell hooks deliver again", "session", sid)
	n.changed("sessions")
	return false
}

// UnreadWaitMax bounds how long GET /unread holds a request (its "wait"
// query, in seconds).
const UnreadWaitMax = 50 * time.Second

// A held GET /unread looks again when the node says something changed
// (Node.UnreadChanged), at most once per unreadWaitGap, and every
// unreadWaitTick besides (what changes by time alone: a claim that lapsed, a
// lease's backoff). Tests shorten them.
var (
	unreadWaitTick = 2 * time.Second
	unreadWaitGap  = 250 * time.Millisecond
)

// changeSignal wakes everyone who waits for the node's next change.
type changeSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait is closed at the next fire.
func (s *changeSignal) wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch == nil {
		s.ch = make(chan struct{})
	}
	return s.ch
}

func (s *changeSignal) fire() {
	s.mu.Lock()
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
	}
	s.mu.Unlock()
}

// UnreadChanged is closed when the node's state next changes (a message came,
// one was read, a lease or a session changed): what a held GET /unread waits
// for (WaitUnread).
func (n *Node) UnreadChanged() <-chan struct{} { return n.unreadSig.wait() }

// WaitUnread answers GET /unread held for the "wait" seconds of q (at most
// UnreadWaitMax; none: at once): get's page as soon as it lists a message,
// else the empty one when the time is up or ctx ends (the client left). It
// asks get again when one of changed (each a node's UnreadChanged) says so,
// and every unreadWaitTick.
func WaitUnread(ctx context.Context, q url.Values, get func() (UnreadPage, error), changed ...func() <-chan struct{}) (UnreadPage, error) {
	wait := time.Duration(0)
	if s := q.Get("wait"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return UnreadPage{}, fmt.Errorf("%w: invalid wait", ErrBadRequest)
		}
		wait = min(time.Duration(v)*time.Second, UnreadWaitMax)
	}
	deadline := time.Now().Add(wait)
	for {
		// Taken before the look: a change during it is not missed.
		signals := make([]<-chan struct{}, 0, len(changed))
		for _, c := range changed {
			signals = append(signals, c())
		}
		asked := time.Now()
		page, err := get()
		if err != nil || page.Total > 0 || !time.Now().Before(deadline) {
			return page, err
		}
		if !sleepUntil(ctx, min(unreadWaitTick, time.Until(deadline)), signals) {
			return page, nil
		}
		// A burst of changes is one look, not one each.
		if gap := unreadWaitGap - time.Since(asked); gap > 0 && !sleepUntil(ctx, gap, nil) {
			return page, nil
		}
	}
}

// sleepUntil waits for d or one of signals, whichever is first; false when
// ctx ended.
func sleepUntil(ctx context.Context, d time.Duration, signals []<-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	woke := make(chan struct{}, len(signals))
	for _, s := range signals {
		go func() {
			select {
			case <-s:
				woke <- struct{}{}
			case <-wctx.Done():
			}
		}()
	}
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-woke:
	}
	return true
}

// ReleaseRequest is the body of POST /release: unread messages session
// claimed and could not deliver (the turn that carried them ended before a
// model request read them).
type ReleaseRequest struct {
	IDs       []string `json:"ids"`
	SessionID string   `json:"session_id"`
}

// Release gives back session's claims and leases of ids: they are unread
// again for their route, and a later delivery (after the lease's backoff, to
// this session or another) takes them. A message the session does not hold
// is left alone.
func (n *Node) Release(req ReleaseRequest) error {
	switch {
	case !validSessionID(req.SessionID):
		return fmt.Errorf("%w: invalid session_id", ErrBadRequest)
	case len(req.IDs) == 0 || len(req.IDs) > 1000:
		return fmt.Errorf("%w: ids required (at most 1000)", ErrBadRequest)
	}
	n.revokeLeases(req.SessionID, req.IDs, "turn_aborted", true)
	return nil
}
