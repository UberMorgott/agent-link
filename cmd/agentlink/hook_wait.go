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
// the session is idle and some arrive it writes the batch to stderr,
// acknowledges it and exits 2; the next Stop arms a new waiter. While the
// session is busy it leaves delivery to the synchronous hooks. Claude Code
// does not deduplicate async hooks, so one waiter per session runs at a time
// (a lock file); it also keeps the idle session registered (heartbeats) and
// ends when the session ends (SessionEnd, or its parent process is gone) or
// before the entry's timeout.

import (
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
}

// defaultWaitOpts: the session's process is the client's agent among the
// waiter's ancestors (agentPID: Claude Code runs it through cmd.exe and the
// plugin's agentlink.cmd, so the parent is not the agent), else the parent.
func defaultWaitOpts(client string) waitOpts {
	ppid := agentPID(client)
	if ppid == 0 {
		ppid = os.Getppid()
	}
	return waitOpts{
		poll:      2 * time.Second,
		heartbeat: 5 * time.Minute,
		life:      agenthook.WaitTimeout*time.Second - 2*time.Minute,
		busyFor:   10 * time.Minute,
		stale:     20 * time.Second,
		alive:     func() bool { return processAlive(ppid) },
	}
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
			_ = hookCall(env.api, http.MethodDelete, "/sessions/"+url.PathEscape(in.SessionID), nil, nil, nil, hookHTTPTimeout)
			return 0
		}
		// Only an idle session's heartbeat: a busy one's hooks keep it
		// registered, and the Stop hook that started this waiter may not have
		// saved its state yet, so "busy" here may be stale and must not undo
		// the idle it just registered (the node would not wake the session).
		if time.Since(lastBeat) >= o.heartbeat && !waiterBusy(st, env.clock(), o.busyFor) {
			req := sessionRequest(client, in.SessionID, folder, true)
			req.Heartbeat = true // keeps the session live, not active (node.Session.LastActive)
			if hookCall(env.api, http.MethodPost, "/sessions", nil, req, nil, hookHTTPTimeout) == nil {
				lastBeat = time.Now()
			}
		}
		if !waiterBusy(st, env.clock(), o.busyFor) && pendingUnread(env, folder, in.SessionID) {
			if code, done := wakeWith(client, in.SessionID, folder, path, stderr, env, o.busyFor); done {
				return code
			}
		}
		time.Sleep(o.poll)
	}
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
func pendingUnread(env hookEnv, folder, session string) bool {
	var page node.UnreadPage
	q := url.Values{"folder": {folder}, "session": {session}, "limit": {"1"}, "actionable": {"1"}, "waiter": {"1"}}
	return hookCall(env.api, http.MethodGet, "/unread", q, nil, &page, hookHTTPTimeout) == nil && page.Total > 0
}

// wakeWith delivers a batch to the idle session under its lock: the batch on
// stderr, then the ack. done is false when there was nothing after all or the
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
	h := &hookSession{env: env, st: &st, sid: sid, folder: folder}
	b, err := h.collect(false, true, false, "")
	if err != nil || b.empty() {
		return 0, false
	}
	text := withNotes(st.Notes, b.text)
	st.Notes = nil
	if _, err := io.WriteString(stderr, text+"\n"); err != nil {
		return 0, false
	}
	h.accept(b)
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

func touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}
