package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"

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
	}
)

// runMCP serves the MCP tools on stdin/stdout until the client leaves. An
// update of the executable does not end it: a client cannot restart a stdio
// server on its own (a subagent cannot at all), and the tools only call the
// local API, so the old process keeps serving until its session ends. The
// updater parks the executable it replaced in a free numbered slot meanwhile.
func runMCP(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ss, err := newMCPServer(cfg).Connect(ctx, mcpStdioTransport(os.Stdin, os.Stdout), nil)
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

// newMCPServer builds the agentlink MCP server on the local API of cfg.
func newMCPServer(cfg config.Config) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: selfupdate.Version}, nil)
	// proj is the project selector: the argument, else the agent's own project.
	proj := func(p string) string { return cmp.Or(p, os.Getenv(envProjectID)) }
	limit := func(n int) int { return cmp.Or(n, 50) }

	addTool(s, "projects", "List the projects of the agentlink app; online/total count the other members, members lists this node too (self: true).", func(mcpNone) (any, error) {
		return listProjects(cfg)
	})
	addTool(s, "members", "List the members of a project, this node first.", func(in mcpProject) (any, error) {
		return listMembers(cfg, proj(in.Project))
	})
	addTool(s, "seats", "List the local agents (seats: Claude Code, Codex sessions) of this node in a project; ask one with send ask_seats.", func(in mcpProject) (any, error) {
		return listSeats(cfg, proj(in.Project))
	})
	addTool(s, "chats", "List chats, most recent first.", func(in mcpChats) (any, error) {
		return listChats(cfg, in.Archive, in.Legacy, proj(in.Project))
	})
	addTool(s, "history", "Messages of a chat, oldest first.", func(in mcpHistory) (any, error) {
		if in.Chat == "" {
			return nil, errors.New("chat is required")
		}
		return history(cfg, in.Chat, limit(in.Limit), in.BeforeSeq, in.AfterSeq, proj(""))
	})
	addTool(s, "unread", "Unread messages for this node, oldest first; next is the cursor of the next page. Does not mark them read.", func(in mcpUnread) (any, error) {
		session, _ := agentSession()
		return unreadForSession(cfg, in.Folder, in.After, limit(in.Limit), proj(in.Project), session, true)
	})
	addToolArgs(s, "send", "Send a message: into a chat (chat), the open chat with a member (to), or a new chat (new_chat_with), optionally with file attachments. Returns {id, chat}, plus hold_reason when the message is held (no agent answers it automatically).", func(in mcpSend, args json.RawMessage) (any, error) {
		key, _ := mcpAskKey("send", args)
		return mcpSendMessage(cfg, in, proj(""), key)
	})
	addToolArgs(s, "discuss", "Ask a local Claude Code or Codex agent in this folder's private agent chat. Each session (and each subagent) has its own chat and thread with that agent, reused on every call and closed when the session ends; topic names another own thread, shared the folder's shared project chat, chat an earlier chat by id, temporary a new one. Local only, separate from any network project for the same folder, waits for the exact agent's reply (default 10m), and returns the reply with project/chat IDs. Use async to post without waiting; timed_out returns IDs for later history lookup; held with hold_reason seat_failed, seat_error and retry_at when the agent cannot answer (e.g. its usage limit), the question staying pending for it.", func(in mcpDiscuss, args json.RawMessage) (any, error) {
		key, _ := mcpAskKey("discuss", args)
		timeout := in.Timeout
		if timeout == "" {
			timeout = "10m"
		}
		return discussMessage(cfg, in.With, in.Body, in.Folder, in.Async, timeout, discussPick{chat: in.Chat, topic: in.Topic, temporary: in.Temporary, shared: in.Shared, askKey: key})
	})
	addTool(s, "ack", "Mark messages read: their authors get read receipts.", func(in mcpAck) (any, error) {
		if len(in.IDs) == 0 {
			return nil, errors.New("ids is required")
		}
		session := in.Session
		if session == "" {
			session, _ = agentSession()
		}
		return ack(cfg, in.Chat, in.IDs, session, proj(in.Project))
	})
	return s
}

// mcpSendMessage validates the routing mode and sends.
func mcpSendMessage(cfg config.Config, in mcpSend, project, askKey string) (any, error) {
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
		info, err := createChat(cfg, in.NewChatWith, "", project)
		if err != nil {
			return nil, err
		}
		a.chat = info.ID
	}
	m, err := sendMessage(cfg, a, in.Ask)
	if err != nil {
		return nil, err
	}
	res := map[string]string{"id": m.ID, "chat": m.ChatID}
	if m.HoldReason != "" {
		res["hold_reason"] = m.HoldReason // held: no agent answers it automatically (node.Hold*)
	}
	return res, nil
}

// addTool registers a tool whose answer is the JSON of f's value as text.
func addTool[In any](s *mcp.Server, name, desc string, f func(In) (any, error)) {
	addToolArgs(s, name, desc, func(in In, _ json.RawMessage) (any, error) { return f(in) })
}

// addToolArgs is addTool whose f also gets the call's raw arguments.
func addToolArgs[In any](s *mcp.Server, name, desc string, f func(In, json.RawMessage) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(_ context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		var args json.RawMessage
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}
		v, err := f(in, args)
		if err != nil {
			if ae, ok := errors.AsType[*apiError](err); ok && ae.msg != "" {
				err = errors.New(ae.msg) // the API's message, verbatim
			}
			return nil, nil, err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(string(data)) == "null" {
			data = []byte("[]") // an empty list
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})
}
