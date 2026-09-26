package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Opening a session in the agent's desktop app (DesktopLauncher). The node
// runs the session's first turn itself, headless, with the messages as its
// prompt, and shows the session in the desktop app by its deep link as soon
// as its id is known (the person watches the turn live):
//
//   - Claude: `claude -p --output-format stream-json --verbose [--resume <id>]`
//     in the folder, the prompt on stdin; its first event (system/init) names
//     the session (claude://resume?session=<id>). The turn succeeded at a
//     result event with is_error false.
//   - Codex: its own `codex app-server` over stdio: initialize, thread/start
//     {cwd} (thread/resume {threadId} for a known last thread), turn/start
//     (then codex://threads/<id>); the process is kept until turn/completed
//     (the turn runs in it). Only status "completed" is a success.
//
// A turn that runs out of time (desktopTurnTimeout) ends with its whole
// process tree (killTree).
//
// Later wakes go the usual way (the session's inbox, `codex queue`).

// DesktopLauncher opens sessions in the agent's desktop app when it is
// installed (its URL protocol is registered) and in Terminal otherwise.
type DesktopLauncher struct {
	// Terminal opens a session when the desktop app cannot (launch_mode
	// terminal, no desktop app).
	Terminal SessionLauncher
	// Version is reported to codex app-server as the client's.
	Version string
}

// desktopTurnTimeout bounds one headless first turn.
const desktopTurnTimeout = 60 * time.Minute

// Launch opens spec in Terminal.
func (l DesktopLauncher) Launch(ctx context.Context, spec LaunchSpec) error {
	if l.Terminal == nil {
		return ErrNoTerminal
	}
	return l.Terminal.Launch(ctx, spec)
}

// Direct reports whether provider's sessions open in its desktop app: its
// URL protocol is registered and its CLI is found.
func (DesktopLauncher) Direct(provider string) bool {
	if provider != ProviderClaude && provider != ProviderCodex {
		return false
	}
	if _, err := exec.LookPath(provider); err != nil {
		return false
	}
	return protocolRegistered(provider)
}

// Run runs spec's first turn headless and opens the session in the desktop
// app. started gets the session (thread) id once the turn has the prompt.
func (l DesktopLauncher) Run(ctx context.Context, spec LaunchSpec, started func(session string)) error {
	bin, err := exec.LookPath(spec.Provider)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoAgent, err)
	}
	// The turn is the person's session now: it outlives the node's context
	// (a seat's turn ends when its seat stops).
	base := context.WithoutCancel(ctx)
	if spec.Seat != "" {
		base = ctx
	}
	ctx, cancel := context.WithTimeout(base, desktopTurnTimeout)
	defer cancel()
	// The app shows the session live, as soon as its id is known.
	var openErr error
	seen := func(id string) {
		started(id)
		if spec.NoOpen {
			return
		}
		if err := openURL(ctx, DeepLink(spec.Provider, id)); err != nil {
			openErr = err
		}
	}
	if spec.Provider == ProviderCodex {
		_, err = l.runCodex(ctx, bin, spec, seen)
	} else {
		_, err = runClaude(ctx, bin, spec, seen)
	}
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrTurnTimeout, err)
	case err != nil:
		return err
	case openErr != nil:
		return fmt.Errorf("%w: %w", ErrOpenApp, openErr)
	}
	return nil
}

// DeepLink is the URL that shows session id in provider's desktop app.
func DeepLink(provider, id string) string {
	if provider == ProviderCodex {
		return "codex://threads/" + url.PathEscape(id)
	}
	return "claude://resume?session=" + url.QueryEscape(id)
}

// ClaudeArgs are the arguments of the headless first turn of spec, with full
// permissions (the prompt goes on stdin: no command-line quoting).
func ClaudeArgs(spec LaunchSpec) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", claudeFullAccess}
	if spec.ResumeID != "" {
		args = append(args, "--resume", spec.ResumeID)
	}
	return args
}

func runClaude(ctx context.Context, bin string, spec LaunchSpec, started func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, bin, ClaudeArgs(spec)...) //nolint:gosec // G204: the agent's CLI, arguments built by ClaudeArgs
	cmd.Dir = spec.Folder
	cmd.Env = append(LaunchEnv(os.Environ()), spec.Env...)
	cmd.Stdin = strings.NewReader(spec.Prompt)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	hideWindow(cmd)
	killTreeOnCancel(ctx, cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	id, serr := ReadClaudeStream(out, started)
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if serr != nil {
		if werr != nil && stderr.Len() > 0 {
			serr = fmt.Errorf("%w: %s", serr, stderr.String())
		}
		return id, serr
	}
	// A successful result is the proof the turn took the prompt; how the
	// process ended after it does not change that.
	return id, nil
}

// killTreeOnCancel makes cmd's context end its whole process tree (the agent
// runs tools as child processes), and not wait for their pipes forever.
func killTreeOnCancel(ctx context.Context, cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killTree(context.WithoutCancel(ctx), cmd.Process) }
	cmd.WaitDelay = 10 * time.Second
}

// ReadClaudeStream reads the events of `claude -p --output-format
// stream-json` until the result: started gets the session id at the first
// event naming it. It returns the session id and the turn's error.
func ReadClaudeStream(r io.Reader, started func(string)) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	id := ""
	for sc.Scan() {
		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
			IsError   bool   `json:"is_error"`
			Result    string `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if id == "" && validSessionID(ev.SessionID) {
			id = ev.SessionID
			started(id)
		}
		if ev.Type == "result" {
			if ev.IsError {
				return id, fmt.Errorf("claude: %s: %s", ev.Subtype, trimMsg(ev.Result))
			}
			return id, nil
		}
	}
	if err := sc.Err(); err != nil {
		return id, err
	}
	if id == "" {
		return "", errors.New("claude: no session started")
	}
	return id, errors.New("claude: ended without a result")
}

func (l DesktopLauncher) runCodex(ctx context.Context, bin string, spec LaunchSpec, started func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, bin, CodexServerArgs(spec)...) //nolint:gosec // G204: the agent's CLI, arguments built by CodexServerArgs
	cmd.Dir = spec.Folder
	cmd.Env = append(LaunchEnv(os.Environ()), spec.Env...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	hideWindow(cmd)
	killTreeOnCancel(ctx, cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	id, terr := CodexTurn(ctx, out, in, spec, l.Version, started)
	_ = in.Close() // app-server ends at the end of its input
	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, out)
		done <- cmd.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = killTree(context.WithoutCancel(ctx), cmd.Process)
		<-done
	}
	if terr != nil && stderr.Len() > 0 {
		terr = fmt.Errorf("%w: %s", terr, stderr.String())
	}
	return id, terr
}

// CodexServerArgs are the arguments of `codex app-server` for spec. Codex
// gives its commands a filtered environment, so spec.Env reaches them through
// shell_environment_policy.set (TOML literal strings; a value with a quote or
// a newline is left out).
func CodexServerArgs(spec LaunchSpec) []string {
	var set []string
	for _, kv := range spec.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k+v, "'\r\n") || strings.ContainsAny(k, " .=\"{}") {
			continue
		}
		set = append(set, k+"='"+v+"'")
	}
	if len(set) == 0 {
		return []string{"app-server"}
	}
	return []string{"app-server", "-c", "shell_environment_policy.set={" + strings.Join(set, ",") + "}"}
}

// rpcMsg is one JSON-RPC message of codex app-server (no "jsonrpc" field).
type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// CodexTurn runs spec's first turn through a codex app-server connection
// (r: its output, w: its input): a new thread in spec.Folder or thread
// spec.ResumeID (a new one when it cannot be resumed), one turn with
// spec.Prompt. started gets the thread id once turn/start is accepted; it
// returns at turn/completed with the thread id. A matching turn/started is
// still required before completion counts as success.
func CodexTurn(ctx context.Context, r io.Reader, w io.Writer, spec LaunchSpec, version string, started func(string)) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the reader
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for sc.Scan() {
			select {
			case lines <- append([]byte(nil), sc.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		readErr <- err
	}()
	next := 0
	send := func(m map[string]any) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	}
	// recv returns the next message, answering the server's own requests (an
	// approval is declined: nobody is there to approve it).
	recv := func() (rpcMsg, error) {
		for {
			select {
			case <-ctx.Done():
				return rpcMsg{}, ctx.Err()
			case err := <-readErr:
				return rpcMsg{}, fmt.Errorf("codex app-server: %w", err)
			case b := <-lines:
				var m rpcMsg
				if json.Unmarshal(b, &m) != nil {
					continue
				}
				if m.Method != "" && len(m.ID) > 0 {
					var reply map[string]any
					if strings.Contains(strings.ToLower(m.Method), "approval") {
						reply = map[string]any{"id": m.ID, "result": map[string]any{"decision": "decline"}}
					} else {
						reply = map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported by agent-link"}}
					}
					if err := send(reply); err != nil {
						return rpcMsg{}, err
					}
					continue
				}
				return m, nil
			}
		}
	}
	// backlog keeps the notifications that came while a call awaited its
	// answer (turn/completed may come before turn/start's): the turn loop
	// reads them first.
	var backlog []rpcMsg
	notification := func() (rpcMsg, error) {
		if len(backlog) > 0 {
			m := backlog[0]
			backlog = backlog[1:]
			return m, nil
		}
		return recv()
	}
	call := func(method string, params any, result any) error {
		next++
		id := next
		if err := send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for {
			m, err := recv()
			if err != nil {
				return err
			}
			if m.Method != "" {
				backlog = append(backlog, m)
				continue
			}
			if string(m.ID) != fmt.Sprint(id) {
				continue
			}
			if m.Error != nil {
				return fmt.Errorf("codex %s: %s", method, m.Error.Message)
			}
			return json.Unmarshal(m.Result, result)
		}
	}
	var none struct{}
	if err := call("initialize", map[string]any{"clientInfo": map[string]any{"name": "agentlink", "title": "agent-link", "version": version}}, &none); err != nil {
		return "", err
	}
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return "", err
	}
	var th struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	// Full permissions (the owner's choice): no approvals, no sandbox.
	full := func(p map[string]any) map[string]any {
		p["approvalPolicy"], p["sandbox"] = "never", "danger-full-access"
		return p
	}
	err := errors.New("no thread")
	if spec.ResumeID != "" {
		// Refused while the desktop app has the thread open (it is its only
		// writer): a new thread then.
		err = call("thread/resume", full(map[string]any{"threadId": spec.ResumeID}), &th)
	}
	if err != nil {
		err = call("thread/start", full(map[string]any{"cwd": spec.Folder}), &th)
	}
	if err != nil {
		return "", err
	}
	tid := th.Thread.ID
	if !validSessionID(tid) {
		return "", fmt.Errorf("codex: bad thread id %q", tid)
	}
	var tr struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	input := []map[string]any{{"type": "text", "text": spec.Prompt}}
	if err := call("turn/start", map[string]any{"threadId": tid, "input": input, "approvalPolicy": "never",
		"sandboxPolicy": map[string]any{"type": "dangerFullAccess"}}, &tr); err != nil {
		return tid, err
	}
	// From here the turn may already be running even if its notification is
	// lost or the connection ends. Mark the session seen so the launch ladder
	// never repeats the same prompt in Terminal.
	started(tid)
	seenStarted := false
	for {
		m, err := notification()
		if err != nil {
			return tid, err
		}
		if m.Method == "turn/started" {
			var began struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			if json.Unmarshal(m.Params, &began) == nil && began.ThreadID == tid &&
				(tr.Turn.ID == "" || began.Turn.ID == tr.Turn.ID) && !seenStarted {
				seenStarted = true
			}
			continue
		}
		if m.Method != "turn/completed" {
			continue
		}
		var done struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(m.Params, &done) != nil || done.ThreadID != tid || (tr.Turn.ID != "" && done.Turn.ID != tr.Turn.ID) {
			continue
		}
		msg := ""
		if done.Turn.Error != nil {
			msg = done.Turn.Error.Message
		}
		if !seenStarted {
			return tid, errors.New("codex turn completed without turn/started")
		}
		switch done.Turn.Status {
		case "completed": // the only success
			return tid, nil
		case "interrupted":
			return tid, fmt.Errorf("codex turn: %w: %s", ErrTurnInterrupted, trimMsg(msg))
		}
		return tid, fmt.Errorf("codex turn %s: %s", done.Turn.Status, trimMsg(msg))
	}
}

func trimMsg(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}

// tailBuffer keeps the last 2 KiB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) Len() int       { return len(t.b) }
func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.b)) }
