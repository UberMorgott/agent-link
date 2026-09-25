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
// prompt, then shows the session in the desktop app by its deep link:
//
//   - Claude: `claude -p --output-format stream-json --verbose [--resume <id>]`
//     in the folder, the prompt on stdin; its first event (system/init) names
//     the session. Then claude://resume?session=<id>.
//   - Codex: its own `codex app-server` over stdio: initialize, thread/start
//     {cwd} (thread/resume {threadId} for a known last thread), turn/start;
//     the process is kept until turn/completed (the turn runs in it). Then
//     codex://threads/<id>.
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
	// The turn is the person's session now: it outlives the node's context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopTurnTimeout)
	defer cancel()
	var id string
	if spec.Provider == ProviderCodex {
		id, err = l.runCodex(ctx, bin, spec, started)
	} else {
		id, err = runClaude(ctx, bin, spec, started)
	}
	if id != "" {
		if oerr := openURL(ctx, DeepLink(spec.Provider, id)); oerr != nil && err == nil {
			err = fmt.Errorf("open desktop app: %w", oerr)
		}
	}
	return err
}

// DeepLink is the URL that shows session id in provider's desktop app.
func DeepLink(provider, id string) string {
	if provider == ProviderCodex {
		return "codex://threads/" + url.PathEscape(id)
	}
	return "claude://resume?session=" + url.QueryEscape(id)
}

// ClaudeArgs are the arguments of the headless first turn of spec (the prompt
// goes on stdin: no command-line quoting).
func ClaudeArgs(spec LaunchSpec) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if spec.ResumeID != "" {
		args = append(args, "--resume", spec.ResumeID)
	}
	return args
}

func runClaude(ctx context.Context, bin string, spec LaunchSpec, started func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, bin, ClaudeArgs(spec)...) //nolint:gosec // G204: the agent's CLI, arguments built by ClaudeArgs
	cmd.Dir = spec.Folder
	cmd.Env = LaunchEnv(os.Environ())
	cmd.Stdin = strings.NewReader(spec.Prompt)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	hideWindow(cmd)
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
	switch {
	case serr != nil:
		return id, serr
	case werr != nil:
		return id, fmt.Errorf("claude: %w: %s", werr, stderr.String())
	}
	return id, nil
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
	cmd := exec.CommandContext(ctx, bin, "app-server")
	cmd.Dir = spec.Folder
	cmd.Env = LaunchEnv(os.Environ())
	var stderr tailBuffer
	cmd.Stderr = &stderr
	hideWindow(cmd)
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
		_ = cmd.Process.Kill()
		<-done
	}
	if terr != nil && stderr.Len() > 0 {
		terr = fmt.Errorf("%w: %s", terr, stderr.String())
	}
	return id, terr
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
// spec.Prompt. started gets the thread id once
// the turn started; it returns at turn/completed with the thread id.
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
			if string(m.ID) != fmt.Sprint(id) || m.Method != "" {
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
	err := errors.New("no thread")
	if spec.ResumeID != "" {
		// Refused while the desktop app has the thread open (it is its only
		// writer): a new thread then.
		err = call("thread/resume", map[string]any{"threadId": spec.ResumeID}, &th)
	}
	if err != nil {
		err = call("thread/start", map[string]any{"cwd": spec.Folder}, &th)
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
	if err := call("turn/start", map[string]any{"threadId": tid, "input": input}, &tr); err != nil {
		return tid, err
	}
	started(tid)
	for {
		m, err := recv()
		if err != nil {
			return tid, err
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
		if done.Turn.Status == "failed" {
			msg := ""
			if done.Turn.Error != nil {
				msg = done.Turn.Error.Message
			}
			return tid, fmt.Errorf("codex turn failed: %s", trimMsg(msg))
		}
		return tid, nil
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
