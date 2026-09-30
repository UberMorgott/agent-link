package main

// agentlink hook claude --wait: Claude Code's background waiter. It is
// installed as an asyncRewake hook on Stop (not SessionStart: Claude Code
// holds a session's start until its SessionStart hooks end): Claude Code runs
// it in the background and, when it exits with code 2, wakes the session even
// when it is idle and shows Claude its stderr
// (https://code.claude.com/docs/en/hooks#command-hook-fields,
// https://code.claude.com/docs/en/hooks#run-hooks-in-the-background).
//
// The waiter polls the node's unread messages of the session's folder. When
// the session is idle and some arrive it claims the batch as a wake (with a
// token), writes it to stderr and exits 2; the session's next hook event
// acknowledges it once the wake is in its transcript, and an unproven wake
// lapses (the messages are delivered again). The next Stop arms a new
// waiter. While the
// session is busy it leaves delivery to the synchronous hooks. Claude Code
// does not deduplicate async hooks, so one waiter per session runs at a time
// (a lock file); it also keeps the idle session registered (heartbeats) and
// ends when the session ends (SessionEnd, or its parent process is gone) or
// before the entry's timeout.
//
// An update of the executable does not end or wake it: exit 2 costs the idle
// model a turn, so it is kept for real messages. The old waiter keeps polling
// the node (old and new versions talk over the same API; the updater parks
// the locked old file), and the session's next Stop starts the new one, which
// asks the old one to hand the session over (takeOver) so it ends then.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/fileutil"
	"github.com/UberMorgott/agent-link/internal/node"
)

// waitOpts are the timings of a waiter (short in tests).
type waitOpts struct {
	poll      time.Duration // how often the node is asked
	heartbeat time.Duration // how often the idle session is re-registered
	life      time.Duration // the waiter ends after this (before the entry's timeout)
	busyFor   time.Duration // a session whose last event was a work event this recently is busy
	// handoff: how long a new waiter waits for the session's running one to
	// hand over (takeOver); 0 tries once.
	handoff time.Duration

	// alive reports whether the session (its agent process) still runs.
	alive func() bool
}

// defaultWaitOpts: the session's process is the client's agent among the
// waiter's ancestors (agentPID: Claude Code runs it through cmd.exe and the
// plugin's agentlink.cmd, so the parent is not the agent).
func defaultWaitOpts(client string) waitOpts {
	return waitOpts{
		poll:      2 * time.Second,
		heartbeat: 5 * time.Minute,
		life:      agenthook.WaitTimeout*time.Second - 2*time.Minute,
		busyFor:   10 * time.Minute,
		handoff:   10 * time.Second,

		alive: agentAlive(agentPID(client)),
	}
}

// agentAlive reports whether the agent process pid still runs; nil when pid
// is unknown (0: no /proc, an unrecognised launcher): an agent not found is
// not an agent gone, so the waiter then ends only with the session
// (SessionEnd) or its life, and never unregisters a live session.
func agentAlive(pid int) func() bool {
	if pid == 0 {
		return nil
	}
	return func() bool { return processAlive(pid) }
}

// hookWait runs the waiter; its exit code is 2 when it delivered a batch on
// stderr (wakes the session), else 0.
func hookWait(client string, stdin io.Reader, stderr io.Writer, env hookEnv, o waitOpts) int {
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil || in.SessionID == "" {
		return 0
	}
	folder := hookFolder(in.Cwd)
	path := hookStatePath(env.dir, client, in.SessionID)
	if os.MkdirAll(env.dir, 0o700) != nil {
		return 0
	}
	release, ok := takeWaitLock(path + ".wait")
	if !ok {
		if release, ok = takeOver(path, o); !ok {
			return 0 // another waiter of this session runs and did not hand over
		}
	}
	defer release()
	start := time.Now()
	_ = os.Remove(path + handoffSuffix) // the request this waiter made, if any
	var lastBeat time.Time
	for {

		st := loadHookState(path)
		if st.Ended || time.Since(start) > o.life || handedOff(path, start) {
			return 0
		}
		if o.alive != nil && !o.alive() {
			_ = hookCall(env.api, http.MethodDelete, "/sessions/"+url.PathEscape(in.SessionID), env.withProject(nil), nil, nil, hookHTTPTimeout)
			return 0
		}
		if time.Since(lastBeat) >= o.heartbeat && keepAlive(env, client, in.SessionID, folder, path) {
			lastBeat = time.Now()
		}
		if !waiterBusy(st, env.clock(), o.busyFor) && pendingUnread(env, folder, in.SessionID, &st) {
			if code, done := wakeWith(client, in.SessionID, folder, path, stderr, env, o.busyFor); done {
				return code
			}
		}
		time.Sleep(o.poll)
	}
}

// keepAlive re-registers the session as a keep-alive with the idle state its
// hooks last told the node (hookState.Idle), read under the session's lock so
// a Stop hook saving its idle cannot race it. The hooks' last event does not
// decide: a busy session whose last event was a SubagentStart or
// SubagentStop is still busy, and a keep-alive that called it idle showed an
// agent at work as waiting for a question. It reports whether the node took it.
func keepAlive(env hookEnv, client, sid, folder, path string) bool {
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return false
	}
	defer unlock()
	st := loadHookState(path)
	if st.Ended || st.Unbound || st.Registered.IsZero() {
		return false
	}
	req := sessionRequest(client, sid, folder, st.Idle, nil) // a keep-alive: the node keeps the subagents the hooks reported
	req.Heartbeat = true                                     // keeps the session live, not active (node.Session.LastActive)
	return hookCall(env.api, http.MethodPost, "/sessions", env.withProject(nil), req, nil, hookHTTPTimeout) == nil
}

// waiterBusy: the session is in a turn (its last event was not Stop or
// SessionStart), so its own hooks deliver.
func waiterBusy(st hookState, now time.Time, busyFor time.Duration) bool {
	switch st.LastEvent {
	case evPrompt, evPreTool, evPostTool:
		return now.Sub(st.LastEventAt) < busyFor
	}
	return false
}

// pendingUnread reports whether actionable unread messages for session wait in folder
// (not those for another session of the folder). It asks as the waiter: a
// node that wakes the session through its inbox answers none.
func pendingUnread(env hookEnv, folder, session string, st *hookState) bool {
	var page node.UnreadPage
	q := url.Values{"folder": {folder}, "session": {session}, "limit": {"1"}, "actionable": {"1"}, "waiter": {"1"}}
	if st != nil {
		st.trackAgent("", "", "", env.clock()) // silent subagents lapse here too
		(&hookSession{st: st}).agentQuery(q)
	}
	q = env.withProject(q)
	return hookCall(env.api, http.MethodGet, "/unread", q, nil, &page, hookHTTPTimeout) == nil && page.Total > 0
}

// wakeWith delivers a batch to the idle session under its lock: claimed as a
// wake, on stderr with the wake's marker. done is false when there was nothing after all or the
// session became busy.
func wakeWith(client, sid, folder, path string, stderr io.Writer, env hookEnv, busyFor time.Duration) (int, bool) {
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return 0, false
	}
	defer unlock()
	st := loadHookState(path)
	if st.Ended || waiterBusy(st, env.clock(), busyFor) {
		return 0, st.Ended
	}
	st.trackAgent("", "", "", env.clock()) // an idle parent has no events: silent subagents lapse here
	token := randomToken()
	h := &hookSession{env: env, st: &st, sid: sid, folder: folder, client: client, wakeToken: token}
	b, err := h.collect(false, true, nil)
	if err != nil || b.empty() {
		return 0, false
	}
	text := withNotes(st.Notes, b.text+"\n"+node.WakeMarker(token))
	st.Notes = nil
	if _, err := io.WriteString(stderr, text+"\n"); err != nil {
		return 0, false
	}
	// Not acknowledged here: stderr is no proof the session got it (it may be
	// gone). The batch is claimed as a wake; the session's next event
	// acknowledges it once the wake is in its transcript (collect), else the
	// claim lapses and the messages are delivered again.
	st.Notice = joinNotice(st.Notice, b.notice) // shown at the session's next event
	st.LastEvent, st.LastEventAt = "wake", env.clock()
	_ = saveHookState(path, st)
	return 2, true
}

// takeWaitLock makes this the session's only waiter: an OS lock held for the
// waiter's life and released with its process, however it ends.
func takeWaitLock(path string) (func(), bool) {
	return fileutil.TryLock(path)
}

// handoffSuffix names a new waiter's request (next to the session's state)
// that the running one hand the session over to it.
const handoffSuffix = ".handoff"

// takeOver asks the session's running waiter to hand over (a request file it
// sees at its next poll, handedOff) and waits up to o.handoff for its lock.
// The session is never without a waiter: the old one ends only once a new one
// asked, so an update's old waiter (still running the parked executable) goes
// at the session's next Stop instead of at the end of its life. A waiter of a
// version before the hand-over ignores the request; this one then gives up and
// takes the request back, leaving nothing behind.
func takeOver(path string, o waitOpts) (func(), bool) {
	req := path + handoffSuffix
	if os.WriteFile(req, nil, 0o600) != nil {
		return nil, false
	}
	deadline := time.Now().Add(o.handoff)
	for {
		if release, ok := takeWaitLock(path + ".wait"); ok {
			return release, true
		}
		if !time.Now().Before(deadline) {
			_ = os.Remove(req)
			return nil, false
		}
		time.Sleep(min(o.poll, 200*time.Millisecond))
	}
}

// handedOff reports that a newer waiter asked for the session after this one
// took it at start.
func handedOff(path string, start time.Time) bool {
	st, err := os.Stat(path + handoffSuffix)
	return err == nil && st.ModTime().After(start)
}

// randomToken is a new wake token (node.WakeMarker).
func randomToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
