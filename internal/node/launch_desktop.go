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
	"path/filepath"
	"slices"
	"strconv"
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
	// ProgramPath resolves a configured executable for each provider.
	ProgramPath func(provider string) string
}

func (l DesktopLauncher) program(provider string) string {
	if l.ProgramPath != nil {
		if path := l.ProgramPath(provider); path != "" {
			return path
		}
	}
	return provider
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
func (l DesktopLauncher) Direct(provider string) bool {
	if provider != ProviderClaude && provider != ProviderCodex {
		return false
	}
	if _, err := exec.LookPath(l.program(provider)); err != nil {
		return false
	}
	return protocolRegistered(provider)
}

// Run runs spec's first turn headless and opens the session in the desktop
// app. started gets the session (thread) id once the turn has the prompt.
func (l DesktopLauncher) Run(ctx context.Context, spec LaunchSpec, started func(session string)) error {
	program := spec.ProgramPath
	if program == "" {
		program = l.program(spec.Provider)
	}
	bin, err := exec.LookPath(program)
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
	id, serr := readClaudeStream(out, started, spec.Doing, spec.Ran, spec.Answer)
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

// claudeDoing follows what a Claude stream's main agent does (its tool calls
// by name) and its running subagents (Task/Agent calls not yet answered).
type claudeDoing struct {
	kind string
	subs map[string]bool
	tell func(kind string, subagents int)
}

func (d *claudeDoing) event(typ, parent string, message json.RawMessage) {
	if d.tell == nil || parent != "" { // a subagent's own events
		return
	}
	var msg struct {
		Content []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	if json.Unmarshal(message, &msg) != nil {
		return // a plain text prompt
	}
	kind, subs := d.kind, len(d.subs)
	for _, c := range msg.Content {
		switch {
		case typ == "assistant" && c.Type == "tool_use":
			kind = ToolKind(c.Name)
			if (c.Name == "Task" || c.Name == "Agent") && c.ID != "" && len(d.subs) < maxAgentSubagents {
				d.subs[c.ID] = true
			}
		case typ == "assistant":
			kind = AgentThinking
		case typ == "user" && c.Type == "tool_result":
			kind = AgentThinking
			delete(d.subs, c.ToolUseID)
		}
	}
	if kind != d.kind || subs != len(d.subs) {
		d.kind = kind
		d.tell(kind, len(d.subs))
	}
}

// ran, when set, gets the model the stream's init event names (Claude does not
// report its reasoning effort there); answer, when set, the text of a
// successful result (the turn's final answer).
func readClaudeStream(r io.Reader, started func(string), doing func(string, int), ran func(string, string), answer func(string)) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	id := ""
	d := claudeDoing{subs: map[string]bool{}, tell: doing}
	for sc.Scan() {
		var ev struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			SessionID string          `json:"session_id"`
			IsError   bool            `json:"is_error"`
			Result    string          `json:"result"`
			Model     string          `json:"model"`
			Parent    string          `json:"parent_tool_use_id"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if id == "" && validSessionID(ev.SessionID) {
			id = ev.SessionID
			started(id)
		}
		if ran != nil && ev.Type == "system" && ev.Subtype == "init" && ev.Model != "" {
			ran(ev.Model, "")
		}
		if ev.Type == "assistant" || ev.Type == "user" {
			d.event(ev.Type, ev.Parent, ev.Message)
		}
		if ev.Type == "result" {
			if ev.IsError {
				return id, fmt.Errorf("claude: %s: %s", ev.Subtype, trimMsg(ev.Result))
			}
			if answer != nil {
				answer(ev.Result)
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
	var id string
	err := runAppServer(ctx, cmd, func(r io.Reader, w io.Writer) error {
		var terr error
		id, terr = CodexTurn(ctx, r, w, spec, l.Version, started)
		return terr
	})
	return id, err
}

// runAppServer starts cmd (a `codex app-server`) and runs fn against its
// output (r) and input (w); the server ends at the end of its input, else 10
// seconds later with its process tree. fn's error carries the server's last
// stderr.
func runAppServer(ctx context.Context, cmd *exec.Cmd, fn func(r io.Reader, w io.Writer) error) error {
	var stderr tailBuffer
	cmd.Stderr = &stderr
	hideWindow(cmd)
	killTreeOnCancel(ctx, cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	ferr := fn(out, in)
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
	if ferr != nil && stderr.Len() > 0 {
		ferr = fmt.Errorf("%w: %s", ferr, stderr.String())
	}
	return ferr
}

// codexMaintTimeout bounds one archival connection to codex app-server
// (ArchiveSessions, ArchiveOrphanSessions).
const codexMaintTimeout = 2 * time.Minute

// codexServer runs fn against a plain `codex app-server` (no MCP server, no
// turn): ErrNoAgent when Codex is not found.
func (l DesktopLauncher) codexServer(ctx context.Context, fn func(ctx context.Context, r io.Reader, w io.Writer) error) error {
	bin, err := exec.LookPath(l.program(ProviderCodex))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoAgent, err)
	}
	ctx, cancel := context.WithTimeout(ctx, codexMaintTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server") //nolint:gosec // G204: the agent's CLI, fixed arguments
	cmd.Env = LaunchEnv(os.Environ())
	return runAppServer(ctx, cmd, func(r io.Reader, w io.Writer) error { return fn(ctx, r, w) })
}

// ArchiveSessions archives provider's sessions ids in its desktop app: Codex
// threads (thread/archive), so the threads of closed seats leave the Codex
// app's sidebar (a person can still unarchive them there). Other providers:
// nothing.
func (l DesktopLauncher) ArchiveSessions(ctx context.Context, provider string, ids []string) error {
	if provider != ProviderCodex || len(ids) == 0 {
		return nil
	}
	return l.codexServer(ctx, func(ctx context.Context, r io.Reader, w io.Writer) error {
		return CodexArchive(ctx, r, w, l.Version, ids)
	})
}

// ArchiveOrphanSessions archives the Codex threads agent-link opened for seats
// (CodexArchiveOrphans) that keep does not keep and that were last updated
// before before; it returns the archived ones.
func (l DesktopLauncher) ArchiveOrphanSessions(ctx context.Context, keep func(id string) bool, before time.Time) ([]string, error) {
	var archived []string
	err := l.codexServer(ctx, func(ctx context.Context, r io.Reader, w io.Writer) error {
		var err error
		archived, err = CodexArchiveOrphans(ctx, r, w, l.Version, keep, before)
		return err
	})
	return archived, err
}

// CodexServerArgs are the arguments of `codex app-server` for spec. Codex
// gives its commands a filtered environment, so spec.Env reaches them through
// shell_environment_policy.set (TOML literal strings; a value with a quote or
// a newline is left out).
// The turn also gets the agent-link MCP server (CodexMCPArgs), so a Codex seat
// can discuss with a Claude seat the way Claude does.
func CodexServerArgs(spec LaunchSpec) []string {
	args := append(CodexMCPArgs(SelfExe(), spec.Env), "app-server")
	if set := tomlEnv(spec.Env, ""); set != "" {
		args = append(args, "-c", "shell_environment_policy.set="+set)
	}
	return args
}

// CodexMCPTimeout is the tool timeout of the agent-link MCP server in a Codex
// run: Codex's default (60 s) would cut a discuss off long before its reply
// wait (at most 15 minutes, cmd/agentlink/mcp.go).
const CodexMCPTimeout = 16 * 60

// CodexMCPArgs are Codex `-c` overrides (global options, placed before the
// subcommand) that add exe's MCP server ("agentlink mcp") to a Codex run.
// Codex starts MCP servers with a filtered environment (PATH, APPDATA and the
// like, not the run's own), so the AGENTLINK_* variables of env (seat,
// project, chat, API) are passed in the server's env table: its discuss and
// send then speak as that seat or job. No exe (not running as agentlink):
// none.
func CodexMCPArgs(exe string, env []string) []string {
	if exe == "" || strings.ContainsAny(exe, "'\r\n") {
		return nil
	}
	const p = "mcp_servers.agentlink."
	args := []string{"-c", p + "command='" + exe + "'", "-c", p + `args=["mcp"]`,
		"-c", p + "tool_timeout_sec=" + strconv.Itoa(CodexMCPTimeout)}
	if set := tomlEnv(env, "AGENTLINK_"); set != "" {
		args = append(args, "-c", p+"env="+set)
	}
	return args
}

// tomlEnv is env's KEY=value entries whose key starts with prefix as a TOML
// inline table of literal strings; an entry a literal cannot hold (a quote or
// a newline) or with an odd key is left out. Empty when none is left.
func tomlEnv(env []string, prefix string) string {
	var set []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || !strings.HasPrefix(k, prefix) || strings.ContainsAny(k+v, "'\r\n") || strings.ContainsAny(k, " .=\"{}") {
			continue
		}
		set = append(set, k+"='"+v+"'")
	}
	if len(set) == 0 {
		return ""
	}
	return "{" + strings.Join(set, ",") + "}"
}

// SelfExe is this program when it is agentlink (the app or its CLI, never a
// plugin's launcher shim), else empty (a test binary).
func SelfExe() string {
	exe, err := os.Executable()
	if err != nil || !strings.HasPrefix(strings.ToLower(filepath.Base(exe)), "agentlink") {
		return ""
	}
	return exe
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
	c := newCodexConn(ctx, r, w)
	call, notification := c.call, c.notification
	if err := c.initialize(version); err != nil {
		return "", err
	}
	var th struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		// The thread's settings for its turns (reasoningEffort null: the
		// model's default).
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	// Full permissions (the owner's choice): no approvals, no sandbox.
	full := func(p map[string]any) map[string]any {
		p["approvalPolicy"], p["sandbox"] = "never", "danger-full-access"
		return p
	}
	err := errors.New("no thread")
	if spec.ResumeID != "" {
		err = call("thread/resume", full(map[string]any{"threadId": spec.ResumeID}), &th)
		if err != nil {
			// An archived thread (its seat's chat closed, or swept as an
			// orphan) is unarchived and resumed: the seat keeps its memory. A
			// thread that is not archived refuses the unarchive.
			var none struct{}
			if uerr := call("thread/unarchive", map[string]any{"threadId": spec.ResumeID}, &none); uerr == nil {
				spec.logInfo("codex: unarchived the thread to resume it", "thread", spec.ResumeID, "resume_err", err)
				err = call("thread/resume", full(map[string]any{"threadId": spec.ResumeID}), &th)
			}
		}
		if err != nil {
			// Refused while the desktop app has the thread open (it is its
			// only writer), or gone: a new thread then.
			spec.logWarn("codex: thread/resume failed; starting a new thread", "thread", spec.ResumeID, "err", err)
		}
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
	if spec.Ran != nil && th.Model != "" {
		spec.Ran(th.Model, th.ReasoningEffort)
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
	doing := ""
	var answer codexAnswer
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
		if m.Method == "item/started" || m.Method == "item/completed" {
			codexDoing(spec.Doing, m.Method, m.Params, &doing)
			if m.Method == "item/completed" {
				answer.item(m.Params, tid, tr.Turn.ID)
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
			if spec.Answer != nil {
				spec.Answer(answer.text)
			}
			return tid, nil
		case "interrupted":
			return tid, fmt.Errorf("codex turn: %w: %s", ErrTurnInterrupted, trimMsg(msg))
		}
		return tid, fmt.Errorf("codex turn %s: %s", done.Turn.Status, trimMsg(msg))
	}
}

// codexConn is a JSON-RPC connection to codex app-server (r: its output, w:
// its input). Its reader ends with ctx.
type codexConn struct {
	ctx     context.Context
	w       io.Writer
	lines   chan []byte
	readErr chan error
	next    int
	// backlog keeps the notifications that came while a call awaited its
	// answer (turn/completed may come before turn/start's): notification
	// returns them first.
	backlog []rpcMsg
}

func newCodexConn(ctx context.Context, r io.Reader, w io.Writer) *codexConn {
	c := &codexConn{ctx: ctx, w: w, lines: make(chan []byte), readErr: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for sc.Scan() {
			select {
			case c.lines <- append([]byte(nil), sc.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		c.readErr <- err
	}()
	return c
}

func (c *codexConn) send(m map[string]any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// recv returns the next message, answering the server's own requests (an
// approval is declined: nobody is there to approve it).
func (c *codexConn) recv() (rpcMsg, error) {
	for {
		select {
		case <-c.ctx.Done():
			return rpcMsg{}, c.ctx.Err()
		case err := <-c.readErr:
			return rpcMsg{}, fmt.Errorf("codex app-server: %w", err)
		case b := <-c.lines:
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
				if err := c.send(reply); err != nil {
					return rpcMsg{}, err
				}
				continue
			}
			return m, nil
		}
	}
}

// notification returns the next notification (the backlog first).
func (c *codexConn) notification() (rpcMsg, error) {
	if len(c.backlog) > 0 {
		m := c.backlog[0]
		c.backlog = c.backlog[1:]
		return m, nil
	}
	return c.recv()
}

// call sends request method and reads its answer into result.
func (c *codexConn) call(method string, params any, result any) error {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		m, err := c.recv()
		if err != nil {
			return err
		}
		if m.Method != "" {
			c.backlog = append(c.backlog, m)
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

// initialize opens the connection as agent-link of version.
func (c *codexConn) initialize(version string) error {
	var none struct{}
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "agentlink", "title": "agent-link", "version": version}}, &none); err != nil {
		return err
	}
	return c.send(map[string]any{"method": "initialized"})
}

// SeatIntroPrefix begins the first prompt of every seat's session (seatIntro):
// a Codex thread whose first message starts with it was opened by agent-link
// for a seat, never by a person.
const SeatIntroPrefix = "agent-link: вы — агент «"

// CodexArchive archives threads ids through a codex app-server connection (r:
// its output, w: its input); the failures are joined.
func CodexArchive(ctx context.Context, r io.Reader, w io.Writer, version string, ids []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := newCodexConn(ctx, r, w)
	if err := c.initialize(version); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		var none struct{}
		if err := c.call("thread/archive", map[string]any{"threadId": id}, &none); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// orphanListPages bounds the thread/list pages CodexArchiveOrphans reads.
const orphanListPages = 50

// CodexArchiveOrphans archives, through a codex app-server connection, the
// unarchived threads agent-link opened for seats (their first message starts
// with SeatIntroPrefix) that keep does not keep (the live seats' sessions) and
// that were last updated before before (a seat's turn may have opened one
// whose id it has not stored yet). A person's own thread never starts with
// the prefix. It returns the archived threads; the failures are joined.
func CodexArchiveOrphans(ctx context.Context, r io.Reader, w io.Writer, version string, keep func(id string) bool, before time.Time) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := newCodexConn(ctx, r, w)
	if err := c.initialize(version); err != nil {
		return nil, err
	}
	// searchTerm narrows the list by title (the first message when the thread
	// has no name); the prefix itself is checked on each.
	search := strings.TrimSuffix(SeatIntroPrefix, " «")
	var orphans []string
	cursor := ""
	for range orphanListPages {
		params := map[string]any{"limit": 100, "searchTerm": search}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data []struct {
				ID        string `json:"id"`
				Preview   string `json:"preview"`
				UpdatedAt int64  `json:"updatedAt"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := c.call("thread/list", params, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Data {
			if strings.HasPrefix(t.Preview, SeatIntroPrefix) && validSessionID(t.ID) && t.UpdatedAt > 0 &&
				time.Unix(t.UpdatedAt, 0).Before(before) && !keep(t.ID) && !slices.Contains(orphans, t.ID) {
				orphans = append(orphans, t.ID)
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" || *page.NextCursor == cursor {
			break
		}
		cursor = *page.NextCursor
	}
	var archived []string
	var errs []error
	for _, id := range orphans {
		var none struct{}
		if err := c.call("thread/archive", map[string]any{"threadId": id}, &none); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		archived = append(archived, id)
	}
	return archived, errors.Join(errs...)
}

// codexAnswer is the final answer of a Codex turn from its completed
// agentMessage items: the latest with phase final_answer, else the latest
// without a phase (a model that does not report it); commentary (progress
// narration) never is.
type codexAnswer struct {
	text  string
	final bool
}

// item takes an item/completed notification of turn turn of thread tid.
func (a *codexAnswer) item(params json.RawMessage, tid, turn string) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Item     struct {
			Type  string  `json:"type"`
			Text  string  `json:"text"`
			Phase *string `json:"phase"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &p) != nil || p.Item.Type != "agentMessage" ||
		(p.ThreadID != "" && p.ThreadID != tid) || (turn != "" && p.TurnID != "" && p.TurnID != turn) {
		return
	}
	switch {
	case p.Item.Phase != nil && *p.Item.Phase == "final_answer":
		a.text, a.final = p.Item.Text, true
	case p.Item.Phase != nil: // commentary (or a phase unknown here): not an answer
	case !a.final:
		a.text = p.Item.Text
	}
}

// codexDoing tells tell what a Codex turn does at an item/started or
// item/completed notification: the item's type (a command, a file change…),
// thinking once it completed. last is the kind told last.
func codexDoing(tell func(string, int), method string, params json.RawMessage, last *string) {
	if tell == nil {
		return
	}
	var p struct {
		Item struct {
			Type   string `json:"type"`
			Server string `json:"server"`
			Tool   string `json:"tool"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	kind := AgentThinking
	if method == "item/started" {
		kind = ToolKind(p.Item.Type)
		if p.Item.Type == "mcpToolCall" && p.Item.Tool == "discuss" && strings.Contains(p.Item.Server, "agentlink") {
			kind = AgentWaiting
		}
	}
	if kind != *last {
		*last = kind
		tell(kind, 0)
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
