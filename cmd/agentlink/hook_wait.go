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

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/node"
)

// waitOpts are the timings of a waiter (short in tests).
type waitOpts struct {
	poll      time.Duration // how often the node is asked
	heartbeat time.Duration // how often the idle session is re-registered
	life      time.Duration // the waiter ends after this (before the entry's timeout)
	busyFor   time.Duration // a session whose last event was a work event this recently is busy
	stale     time.Duration // a waiter lock not refreshed for this long is stale
	// alive reports whether the session (its agent process) still runs.
	alive func() bool
	// replaced reports whether a newer executable occupies this process's launch path.
	replaced func() bool
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
		stale:     20 * time.Second,
		alive:     agentAlive(agentPID(client)),
		replaced:  executableReplaced(),
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
	release, ok := takeWaitLock(path+".wait", o.stale)
	if !ok {
		return 0 // another waiter of this session runs
	}
	defer release()
	start := time.Now()
	var lastBeat time.Time
	for {
		touch(path + ".wait")
		st := loadHookState(path)
		if st.Ended || time.Since(start) > o.life {
			return 0
		}
		if o.alive != nil && !o.alive() {
			_ = hookCall(env.api, http.MethodDelete, "/sessions/"+url.PathEscape(in.SessionID), env.withProject(nil), nil, nil, hookHTTPTimeout)
			return 0
		}
		if o.replaced != nil && o.replaced() {
			// Stop can launch this waiter before it saves idle state. Wait for
			// that state, instead of exiting silently and stranding an idle
			// session. An actual running turn also gets its own next Stop.
			if !waiterBusy(st, env.clock(), o.busyFor) {
				// Claude's asyncRewake treats exit 2 plus stderr as a new turn;
				// its next Stop launches the current executable. No unread message
				// is claimed here, so ordinary delivery remains intact.
				_, _ = io.WriteString(stderr, "AgentLink was updated. Finish this turn so the Stop hook starts the updated message waiter.\n")
				return 2
			}
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

// takeWaitLock makes this the session's only waiter; a lock not refreshed
// within stale belongs to a waiter that is gone.
func takeWaitLock(path string, stale time.Duration) (func(), bool) {
	for range 2 {
		f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		st, serr := os.Stat(path)
		if serr != nil || time.Since(st.ModTime()) <= stale {
			return nil, false
		}
		_ = os.Remove(path)
	}
	return nil, false
}

// randomToken is a new wake token (node.WakeMarker).
func randomToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}
