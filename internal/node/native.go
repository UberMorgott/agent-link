package node

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// What the node does for a session whose client delivers in-process (Claude
// Code's hooks module, SessionRequest.Native) instead of through shell hooks:
// it holds the module's unread query until there is something (WaitUnread),
// takes back what a turn that never ran had claimed (Release), and keeps the
// shell hooks of the same session out (sessionNative).

// sessionNative reports whether live session sid is delivered to by its
// in-process module: its shell hooks and their waiter get nothing then.
func (n *Node) sessionNative(sid string) bool {
	if sid == "" {
		return false
	}
	r := n.sess
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sid]
	return s != nil && s.Native && s.live(time.Now())
}

// UnreadWaitMax bounds how long GET /unread holds a request (its "wait"
// query, in seconds).
const UnreadWaitMax = 50 * time.Second

// unreadWaitTick is how often a held GET /unread looks again; tests shorten it.
var unreadWaitTick = 250 * time.Millisecond

// WaitUnread answers GET /unread held for the "wait" seconds of q (at most
// UnreadWaitMax; none: at once): get's page as soon as it lists a message,
// else the empty one when the time is up or ctx ends (the client left).
func WaitUnread(ctx context.Context, q url.Values, get func() (UnreadPage, error)) (UnreadPage, error) {
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
		page, err := get()
		if err != nil || page.Total > 0 || !time.Now().Before(deadline) {
			return page, err
		}
		t := time.NewTimer(unreadWaitTick)
		select {
		case <-ctx.Done():
			t.Stop()
			return page, nil
		case <-t.C:
		}
	}
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
