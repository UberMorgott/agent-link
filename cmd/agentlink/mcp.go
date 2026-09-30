package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
)

// agentlink mcp is a stdio MCP server for one agent session: its tools call
// the node's local API through the same client code as the CLI commands, and
// answer with the CLI's JSON (a list as one JSON array). An API error is a
// tool error carrying the API's message.

const mcpServerName = "agentlink"

// mcpStdioTransport serves on reader and writer without closing them. The
// SDK's StdioTransport closes os.Stdin on shutdown, and on Windows that Close
// can block behind the outstanding pipe read; the SDK stops its read loop when
// the transport closes anyway.
func mcpStdioTransport(reader io.Reader, writer io.Writer) *mcp.IOTransport {
	return &mcp.IOTransport{Reader: io.NopCloser(reader), Writer: nopWriteCloser{writer}}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type (
	mcpNone    struct{}
	mcpProject struct {
		Project string `json:"project,omitempty" jsonschema:"project id (or legacy); default: $AGENTLINK_PROJECT_ID, else this folder's project"`
	}
	mcpChats struct {
		Project string `json:"project,omitempty" jsonschema:"project id (or legacy); default: $AGENTLINK_PROJECT_ID, else every project's chats"`
		Archive bool   `json:"archive,omitempty" jsonschema:"list the archive instead of the main list"`
		Legacy  bool   `json:"legacy,omitempty" jsonschema:"add history from before chats as virtual chats"`
	}
	mcpHistory struct {
		Chat      string `json:"chat" jsonschema:"chat id"`
		Limit     int    `json:"limit,omitempty" jsonschema:"maximum messages (default 50)"`
		BeforeSeq uint64 `json:"before_seq,omitempty" jsonschema:"only messages before this seq"`
		AfterSeq  uint64 `json:"after_seq,omitempty" jsonschema:"only messages after this seq, oldest first"`
	}
	mcpUnread struct {
		Folder  string `json:"folder,omitempty" jsonschema:"only messages for a session in this folder (default: all)"`
		Project string `json:"project,omitempty" jsonschema:"project id (or legacy); default: $AGENTLINK_PROJECT_ID, else this folder's project"`
		Limit   int    `json:"limit,omitempty" jsonschema:"maximum messages (default 50)"`
		After   string `json:"after,omitempty" jsonschema:"cursor of the last message of the previous page (next of the previous answer)"`
	}
	mcpSend struct {
		Chat        string   `json:"chat,omitempty" jsonschema:"chat id to write in; exactly one of chat, to, new_chat_with"`
		To          string   `json:"to,omitempty" jsonschema:"member name (or area:NAME): the one open chat with them; exactly one of chat, to, new_chat_with"`
		NewChatWith []string `json:"new_chat_with,omitempty" jsonschema:"members to open a new chat with; exactly one of chat, to, new_chat_with"`
		Body        string   `json:"body,omitempty" jsonschema:"message text (may be empty when attachments are given)"`
		Ask         []string `json:"ask,omitempty" jsonschema:"participants who must answer; none: the message only informs"`
		AskSeats    []string `json:"ask_seats,omitempty" jsonschema:"local agents (seats) of this node who must answer: labels or ids (see the seats tool), or all"`
		ReplyTo     string   `json:"reply_to,omitempty" jsonschema:"id of the message being answered"`
		Attachments []string `json:"attachments,omitempty" jsonschema:"absolute paths of files to attach (at most 10): images png/jpeg/gif/webp, pdf or utf-8 text, each at most 10 MB, inside the project folder or the temp folder"`
	}
	mcpAck struct {
		IDs     []string `json:"ids" jsonschema:"message ids to mark read"`
		Chat    string   `json:"chat,omitempty" jsonschema:"chat id (default: any chat, and plain messages)"`
		Project string   `json:"project,omitempty" jsonschema:"project id (or legacy); default: $AGENTLINK_PROJECT_ID"`
		Session string   `json:"session,omitempty" jsonschema:"the reading session's id; default: this agent session"`
	}
	mcpDiscuss struct {
		With    string `json:"with" jsonschema:"local agent to ask: claude or codex"`
		Body    string `json:"body" jsonschema:"message for the agent, in this session's own chat with it"`
		Folder  string `json:"folder,omitempty" jsonschema:"project working folder, default current folder"`
		Async   bool   `json:"async,omitempty" jsonschema:"return message IDs immediately instead of waiting for the agent's reply"`
		Timeout string `json:"timeout,omitempty" jsonschema:"maximum time to wait for the reply, default 10m, at most 15m"`
		Chat    string `json:"chat,omitempty" jsonschema:"continue this local chat: the chat id an earlier discuss returned"`
		Topic   string `json:"topic,omitempty" jsonschema:"named chat: this session's own thread of that name (with shared: the project's persistent chat of that name)"`
		// Temporary starts a new chat; later calls pass its id as chat.
		Temporary bool `json:"temporary,omitempty" jsonschema:"start a new temporary chat, removed when this session ends; pass its id as chat afterwards"`
		// Shared asks in the project's shared chat instead of the session's own.
		Shared bool `json:"shared,omitempty" jsonschema:"ask in the folder's shared project chat (or the topic's shared chat) instead of this session's own chat"`
		// Full answers with the whole result instead of the compact one.
		Full bool `json:"full,omitempty" jsonschema:"return the whole result with the full reply message (default: compact: chat, id, reply text, reply_id, from, model, effort and status)"`
	}
)

// runMCP serves the MCP tools on stdin/stdout until the client leaves. An
// update of the executable does not end it: a client cannot restart a stdio
// server on its own (a subagent cannot at all), so the old process keeps
// serving until its session ends. The updater parks the executable it
// replaced in a free numbered slot meanwhile, and each tool call runs in the
// new executable (delegateMCP): the session gets the updated tools without a
// restart.
func runMCP(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ss, err := newMCPTools(cfg, freshExecutable()).s.Connect(ctx, mcpStdioTransport(os.Stdin, os.Stdout), nil)
	if err != nil {
		return err
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- ss.Wait() }()
	select {
	case err := <-waitDone:
		return err
	case <-ctx.Done():
		_ = ss.Close()
		return ctx.Err()
	}
}

// mcpTools is the agentlink MCP server and each of its tools' call by name
// (mcp-call runs one).
type mcpTools struct {
	s     *mcp.Server
	api   string // the local API the tools call
	calls map[string]func(context.Context, json.RawMessage) (any, error)
	// fresh, when set, names the executable that replaced this one at its
	// path since it started ("" while none did): the calls run there.
	fresh func() string
}

// newMCPServer builds the agentlink MCP server on the local API of cfg.
func newMCPServer(cfg config.Config) *mcp.Server { return newMCPTools(cfg, nil).s }

// newMCPTools builds the agentlink MCP server on the local API of cfg, its
// calls going to the fresh executable once there is one.
func newMCPTools(cfg config.Config, fresh func() string) *mcpTools {
	s := &mcpTools{s: mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: selfupdate.Version}, nil),
		api: cfg.API, calls: map[string]func(context.Context, json.RawMessage) (any, error){}, fresh: fresh}
	// proj is the project selector: the argument, else the agent's own project.
	proj := func(p string) string { return cmp.Or(p, os.Getenv(envProjectID)) }
	limit := func(n int) int { return cmp.Or(n, 50) }

	addTool(s, "projects", "List the projects of the agentlink app; online/total count the other members, members lists this node too (self: true).", func(ctx context.Context, _ mcpNone) (any, error) {
		return listProjects(ctx, cfg)
	})
	addTool(s, "members", "List the members of a project, this node first.", func(ctx context.Context, in mcpProject) (any, error) {
		return listMembers(ctx, cfg, proj(in.Project))
	})
	addTool(s, "seats", "List the local agents (seats: Claude Code, Codex sessions) of this node in a project; ask one with send ask_seats.", func(ctx context.Context, in mcpProject) (any, error) {
		return listSeats(ctx, cfg, proj(in.Project))
	})
	addTool(s, "chats", "List chats, most recent first.", func(ctx context.Context, in mcpChats) (any, error) {
		return listChats(ctx, cfg, in.Archive, in.Legacy, proj(in.Project))
	})
	addTool(s, "history", "Messages of a chat, oldest first.", func(ctx context.Context, in mcpHistory) (any, error) {
		if in.Chat == "" {
			return nil, errors.New("chat is required")
		}
		return history(ctx, cfg, in.Chat, limit(in.Limit), in.BeforeSeq, in.AfterSeq, proj(""))
	})
	addTool(s, "unread", "Unread messages for this node, oldest first; next is the cursor of the next page. Does not mark them read.", func(ctx context.Context, in mcpUnread) (any, error) {
		session, _ := agentSession()
		return unreadForSession(ctx, cfg, in.Folder, in.After, limit(in.Limit), proj(in.Project), session, true)
	})
	addToolArgs(s, "send", "Send a message: into a chat (chat), the open chat with a member (to), or a new chat (new_chat_with), optionally with file attachments. Returns {id, chat}, plus hold_reason when the message is held (no agent answers it automatically).", func(ctx context.Context, in mcpSend, args json.RawMessage) (any, error) {
		key, _ := mcpAskKey("send", args)
		return mcpSendMessage(ctx, cfg, in, proj(""), key)
	})
	addToolArgs(s, "discuss", "Ask a local Claude Code or Codex agent in this folder's private agent chat. Each session (and each subagent) has its own chat and thread with that agent, reused on every call and closed when the session ends; topic names another own thread, shared the folder's shared project chat, chat an earlier chat by id, temporary a new one. Local only, separate from any network project for the same folder, waits for the exact agent's reply (default 10m), and returns the reply compactly: chat, id (the question), reply (its text), reply_id, from, model and effort the agent ran with (full: the whole reply message and project). Use async to post without waiting; timed_out returns IDs for later history lookup; held with hold_reason seat_failed, seat_error and retry_at when the agent cannot answer (e.g. its usage limit), the question staying pending for it.", func(ctx context.Context, in mcpDiscuss, args json.RawMessage) (any, error) {
		key, _ := mcpAskKey("discuss", args)
		timeout := in.Timeout
		if timeout == "" {
			timeout = "10m"
		}
		r, err := discussMessage(ctx, cfg, in.With, in.Body, in.Folder, in.Async, timeout, discussPick{chat: in.Chat, topic: in.Topic, temporary: in.Temporary, shared: in.Shared, askKey: key})
		if err != nil {
			return r, err
		}
		// The answer is on its way to the caller: its reply is read now (best
		// effort; a failed ack leaves it for the hooks to deliver again).
		_ = ackDiscussReply(ctx, cfg, r)
		if in.Full {
			return r, nil
		}
		return compactDiscuss(r), nil
	})
	addTool(s, "ack", "Mark messages read: their authors get read receipts.", func(ctx context.Context, in mcpAck) (any, error) {
		if len(in.IDs) == 0 {
			return nil, errors.New("ids is required")
		}
		session := in.Session
		if session == "" {
			session, _ = agentSession()
		}
		return ack(ctx, cfg, in.Chat, in.IDs, session, proj(in.Project))
	})
	return s
}

// freshExecutable is the executable that replaced this one at its path since
// it started ("" while none did).
func freshExecutable() func() string {
	path, err := os.Executable()
	if err != nil {
		return func() string { return "" }
	}
	replaced := fileReplaced(path)
	return func() string {
		if replaced() {
			return path
		}
		return ""
	}
}

// mcpCallStarted is the first line `agentlink mcp-call` prints, before it
// runs the call; mcpCallResult the last: OK with the call's text, or not OK
// with its error.
const mcpCallStarted = `{"started":true}`

type mcpCallResult struct {
	OK    bool   `json:"ok"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// mcpCallArgs are the arguments that run tool in an executable (a test
// replaces them).
var mcpCallArgs = func(tool string) []string { return []string{"mcp-call", "--tool", tool} }

// delegateMCP runs tool with args in exe (`agentlink mcp-call`) on the local
// API api (this server's, never the new process's own default). ran is false
// only when exe never started the call (not runnable, or no mcp-call: no
// started line): the caller runs it itself then. A call that started is never
// run twice: without its result it is an error (a send may have gone out).
func delegateMCP(ctx context.Context, exe, api, tool string, args json.RawMessage) (text string, ran bool, err error) {
	cmd := exec.CommandContext(ctx, exe, mcpCallArgs(tool)...) //nolint:gosec // G204: this program's own updated executable
	cmd.Stdin = bytes.NewReader(args)
	cmd.Env = os.Environ()
	if api != "" {
		cmd.Env = append(cmd.Env, envAPI+"="+api)
	}
	var out bytes.Buffer
	var stderr tailWriter
	cmd.Stdout, cmd.Stderr = &out, &stderr
	hideConsole(cmd)
	werr := cmd.Run() // the printed result counts, not the exit code
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if strings.TrimSpace(lines[0]) != mcpCallStarted {
		return "", false, nil
	}
	var r mcpCallResult
	parsed := len(lines) >= 2 && json.Unmarshal([]byte(lines[len(lines)-1]), &r) == nil
	switch {
	case parsed && r.Error != "":
		return "", true, errors.New(r.Error) // the tool's own error (an older mcp-call said ok with it)
	case !parsed || !r.OK:
		return "", true, fmt.Errorf("the updated agentlink (%s) did not finish %s (%w): %s", exe, tool, cmp.Or(werr, errNoResult), strings.TrimSpace(stderr.String()))
	}
	return r.Text, true, nil
}

// errNoResult: a delegated call started and ended without printing its result.
var errNoResult = errors.New("no result")

// tailWriter keeps the last 1 KiB written to it.
type tailWriter struct{ b []byte }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 1024 {
		t.b = t.b[len(t.b)-1024:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string { return string(t.b) }

// runMCPCall runs one MCP tool, its arguments JSON on in, printing
// mcpCallStarted before and its mcpCallResult after: what an MCP server
// whose executable was updated calls.
func runMCPCall(ctx context.Context, cfg config.Config, tool string, in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, maxBodyFile+1))
	if err != nil {
		return err
	}
	if _, err := io.WriteString(out, mcpCallStarted+"\n"); err != nil {
		return err
	}
	var r mcpCallResult
	if call := newMCPTools(cfg, nil).calls[tool]; call == nil {
		r.Error = "unknown tool " + tool
	} else if r.Text, err = mcpText(call(ctx, raw)); err != nil {
		r.Error = err.Error()
	}
	r.OK = r.Error == ""
	return json.NewEncoder(out).Encode(r)
}

// mcpText is a tool's answer as its text: v's JSON ("[]" for none), or err
// with the API's own message.
func mcpText(v any, err error) (string, error) {
	if err != nil {
		if ae, ok := errors.AsType[*apiError](err); ok && ae.msg != "" {
			err = errors.New(ae.msg) // the API's message, verbatim
		}
		return "", err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "null" {
		data = []byte("[]") // an empty list
	}
	return string(data), nil
}

// mcpSendMessage validates the routing mode and sends.
func mcpSendMessage(ctx context.Context, cfg config.Config, in mcpSend, project, askKey string) (any, error) {
	modes := 0
	for _, set := range []bool{in.Chat != "", in.To != "", len(in.NewChatWith) > 0} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return nil, errors.New("exactly one of chat, to, new_chat_with is required")
	}
	if in.Body == "" && len(in.Attachments) == 0 {
		return nil, errors.New("body is required (or attachments)")
	}
	a := sendArgs{to: in.To, body: in.Body, replyTo: in.ReplyTo, chat: in.Chat, project: project, files: in.Attachments, askSeats: in.AskSeats, askKey: askKey}
	if len(in.NewChatWith) > 0 {
		info, err := createChat(ctx, cfg, in.NewChatWith, "", project)
		if err != nil {
			return nil, err
		}
		a.chat = info.ID
	}
	m, err := sendMessage(ctx, cfg, a, in.Ask)
	if err != nil {
		return nil, err
	}
	res := map[string]string{"id": m.ID, "chat": m.ChatID}
	if m.HoldReason != "" {
		res["hold_reason"] = m.HoldReason // held: no agent answers it automatically (node.Hold*)
	}
	return res, nil
}

// discussBrief is the compact result of a discuss: the reply's text and who
// wrote it with what, the IDs to continue or look it up, and any status.
type discussBrief struct {
	Chat        string    `json:"chat"`
	ID          string    `json:"id,omitempty"` // the question
	Reply       string    `json:"reply,omitempty"`
	ReplyID     string    `json:"reply_id,omitempty"`
	From        string    `json:"from,omitempty"`
	Model       string    `json:"model,omitempty"`
	Effort      string    `json:"effort,omitempty"`
	Attachments []string  `json:"attachments,omitempty"`
	Queued      bool      `json:"queued,omitempty"`
	TimedOut    bool      `json:"timed_out,omitempty"`
	Held        bool      `json:"held,omitempty"`
	HoldReason  string    `json:"hold_reason,omitempty"`
	Note        string    `json:"note,omitempty"`
	SeatError   string    `json:"seat_error,omitempty"`
	RetryAt     time.Time `json:"retry_at,omitzero"`
}

// compactDiscuss is r without what an asker rarely needs (the reply's
// routing and bookkeeping fields, the project, scope and topic).
func compactDiscuss(r discussResult) discussBrief {
	b := discussBrief{Chat: r.Chat, ID: r.ID, Queued: r.Queued, TimedOut: r.TimedOut, Held: r.Held,
		HoldReason: r.HoldReason, Note: r.Note, SeatError: r.SeatError, RetryAt: r.RetryAt}
	if m := r.Reply; m != nil {
		b.Reply, b.ReplyID, b.From = m.Body, m.ID, m.From
		if m.Agent != nil {
			b.From, b.Model, b.Effort = cmp.Or(m.Agent.Label, m.Agent.Provider), m.Agent.Model, m.Agent.Effort
		}
		for _, a := range m.Attachments {
			if a.Path != "" {
				b.Attachments = append(b.Attachments, a.Path)
			}
		}
	}
	return b
}

// addTool registers a tool whose answer is the JSON of f's value as text.
func addTool[In any](s *mcpTools, name, desc string, f func(context.Context, In) (any, error)) {
	addToolArgs(s, name, desc, func(ctx context.Context, in In, _ json.RawMessage) (any, error) { return f(ctx, in) })
}

// addToolArgs is addTool whose f also gets the call's raw arguments. f's
// context ends with the call (the client cancelled it, the session ended).
// Once the executable was updated, the call runs in the new one
// (delegateMCP).
func addToolArgs[In any](s *mcpTools, name, desc string, f func(context.Context, In, json.RawMessage) (any, error)) {
	s.calls[name] = func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in In
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
		}
		return f(ctx, in, raw)
	}
	mcp.AddTool(s.s, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		var args json.RawMessage
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}
		if s.fresh != nil {
			if exe := s.fresh(); exe != "" {
				if text, ran, err := delegateMCP(ctx, exe, s.api, name, args); ran {
					if err != nil {
						return nil, nil, err
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
				}
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
			}
		}
		text, err := mcpText(f(ctx, in, args))
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}
