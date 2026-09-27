package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
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

const mcpRetirementInterval = 2 * time.Second
const mcpRestartMessage = "AgentLink was updated; restart this MCP session"

// mcpLifecycle tracks inbound JSON-RPC request IDs until their matching
// responses have been written. A handler returning is not enough: the SDK
// still needs to encode and write its response.
type mcpLifecycle struct {
	mu       sync.Mutex
	requests map[string]int
	partial  []byte
	retiring bool
	changed  chan struct{}
}

func newMCPLifecycle() *mcpLifecycle {
	return &mcpLifecycle{requests: make(map[string]int), changed: make(chan struct{}, 1)}
}

func (a *mcpLifecycle) notify() {
	select {
	case a.changed <- struct{}{}:
	default:
	}
}

func (a *mcpLifecycle) begin() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.retiring
}

func (a *mcpLifecycle) read(p []byte) {
	a.mu.Lock()
	a.partial = append(a.partial, p...)
	for {
		end := bytes.IndexByte(a.partial, '\n')
		if end < 0 {
			break
		}
		for _, id := range mcpRequestIDs(a.partial[:end]) {
			a.requests[id]++
		}
		a.partial = a.partial[end+1:]
	}
	a.mu.Unlock()
	a.notify()
}

func (a *mcpLifecycle) written(p []byte) {
	a.mu.Lock()
	for line := range bytes.SplitSeq(p, []byte{'\n'}) {
		for _, id := range mcpResponseIDs(line) {
			if a.requests[id] > 1 {
				a.requests[id]--
			} else {
				delete(a.requests, id)
			}
		}
	}
	a.mu.Unlock()
	a.notify()
}

func (a *mcpLifecycle) waitForRetirement(ctx context.Context) error {
	a.mu.Lock()
	a.retiring = true
	a.mu.Unlock()
	for {
		a.mu.Lock()
		ready := len(a.requests) == 0
		a.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.changed:
		}
	}
}

func mcpRequestIDs(data []byte) []string  { return mcpFrameIDs(data, true) }
func mcpResponseIDs(data []byte) []string { return mcpFrameIDs(data, false) }

func mcpFrameIDs(data []byte, requests bool) []string {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var batch []json.RawMessage
	if json.Unmarshal(data, &batch) == nil {
		var ids []string
		for _, item := range batch {
			ids = append(ids, mcpFrameIDs(item, requests)...)
		}
		return ids
	}
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(data, &frame) != nil || (frame.Method != "") != requests {
		return nil
	}
	var id bytes.Buffer
	if len(frame.ID) == 0 || bytes.Equal(frame.ID, []byte("null")) || json.Compact(&id, frame.ID) != nil {
		return nil
	}
	return []string{id.String()}
}

func watchMCPReplacement(ctx context.Context, replaced func() bool, interval time.Duration, updated chan<- struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if replaced() {
				select {
				case updated <- struct{}{}:
				case <-ctx.Done():
				}
				return
			}
		}
	}
}

// The SDK's StdioTransport closes os.Stdin during shutdown. On Windows, Close
// can block behind an outstanding pipe read, leaving the old MCP process alive
// after an update. The SDK still stops its own read loop when IOTransport is
// closed, so stdin and stdout need not be closed here.
func mcpStdioTransport(reader io.Reader, writer io.Writer, activity *mcpLifecycle) *mcp.IOTransport {
	if activity == nil {
		return &mcp.IOTransport{Reader: io.NopCloser(reader), Writer: mcpResultWriter{Writer: writer}}
	}
	return &mcp.IOTransport{Reader: io.NopCloser(&mcpRequestReader{Reader: reader, activity: activity}), Writer: mcpResultWriter{Writer: writer, activity: activity}}
}

type mcpRequestReader struct {
	io.Reader
	activity *mcpLifecycle
}

func (r *mcpRequestReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.activity.read(p[:n])
	}
	return n, err
}

type mcpResultWriter struct {
	io.Writer
	activity *mcpLifecycle
}

func (w mcpResultWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n == len(p) && w.activity != nil {
		w.activity.written(p)
	}
	return n, err
}

func (mcpResultWriter) Close() error { return nil }

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
		Body    string `json:"body" jsonschema:"message for the agent in this folder's one project chat"`
		Folder  string `json:"folder,omitempty" jsonschema:"project working folder, default current folder"`
		Async   bool   `json:"async,omitempty" jsonschema:"return message IDs immediately instead of waiting for the agent's reply"`
		Timeout string `json:"timeout,omitempty" jsonschema:"maximum time to wait for the reply, default 10m, at most 15m"`
	}
)

// runMCP serves the MCP tools on stdin/stdout until the client leaves.
func runMCP(cfg config.Config) error {
	replaced := executableReplaced()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	activity := newMCPLifecycle()
	ss, err := newMCPServerWithActivity(cfg, activity).Connect(ctx, mcpStdioTransport(os.Stdin, os.Stdout, activity), nil)
	if err != nil {
		return err
	}
	updated := make(chan struct{}, 1)
	go watchMCPReplacement(ctx, replaced, mcpRetirementInterval, updated)
	waitDone := make(chan error, 1)
	go func() { waitDone <- ss.Wait() }()
	select {
	case err := <-waitDone:
		return err
	case <-ctx.Done():
		_ = ss.Close()
		return ctx.Err()
	case <-updated:
		// The SDK's Close sends EOF before an active tool's response reaches the
		// client. Wait for each active result write, then exit the old process.
		return activity.waitForRetirement(ctx)
	}
}

// newMCPServer builds the agentlink MCP server on the local API of cfg.
func newMCPServer(cfg config.Config) *mcp.Server {
	return newMCPServerWithActivity(cfg, nil)
}

func newMCPServerWithActivity(cfg config.Config, activity *mcpLifecycle) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: selfupdate.Version}, nil)
	// proj is the project selector: the argument, else the agent's own project.
	proj := func(p string) string { return cmp.Or(p, os.Getenv(envProjectID)) }
	limit := func(n int) int { return cmp.Or(n, 50) }

	addTool(s, activity, "projects", "List the projects of the agentlink app; online/total count the other members, members lists this node too (self: true).", func(mcpNone) (any, error) {
		return listProjects(cfg)
	})
	addTool(s, activity, "members", "List the members of a project, this node first.", func(in mcpProject) (any, error) {
		return listMembers(cfg, proj(in.Project))
	})
	addTool(s, activity, "seats", "List the local agents (seats: Claude Code, Codex sessions) of this node in a project; ask one with send ask_seats.", func(in mcpProject) (any, error) {
		return listSeats(cfg, proj(in.Project))
	})
	addTool(s, activity, "chats", "List chats, most recent first.", func(in mcpChats) (any, error) {
		return listChats(cfg, in.Archive, in.Legacy, proj(in.Project))
	})
	addTool(s, activity, "history", "Messages of a chat, oldest first.", func(in mcpHistory) (any, error) {
		if in.Chat == "" {
			return nil, errors.New("chat is required")
		}
		return history(cfg, in.Chat, limit(in.Limit), in.BeforeSeq, in.AfterSeq, proj(""))
	})
	addTool(s, activity, "unread", "Unread messages for this node, oldest first; next is the cursor of the next page. Does not mark them read.", func(in mcpUnread) (any, error) {
		session, _ := agentSession()
		return unreadForSession(cfg, in.Folder, in.After, limit(in.Limit), proj(in.Project), session, true)
	})
	addTool(s, activity, "send", "Send a message: into a chat (chat), the open chat with a member (to), or a new chat (new_chat_with), optionally with file attachments. Returns {id, chat}.", func(in mcpSend) (any, error) {
		return mcpSendMessage(cfg, in, proj(""))
	})
	addTool(s, activity, "discuss", "Ask a local Claude Code or Codex agent in this folder's private agent chat. Creates or reuses a local project separate from any network project for the same folder, waits for the exact agent's reply (default 10m), and returns the reply with project/chat IDs. Use async to post without waiting; timed_out returns IDs for later history lookup.", func(in mcpDiscuss) (any, error) {
		timeout := in.Timeout
		if timeout == "" {
			timeout = "10m"
		}
		return discussMessage(cfg, in.With, in.Body, in.Folder, in.Async, timeout)
	})
	addTool(s, activity, "ack", "Mark messages read: their authors get read receipts.", func(in mcpAck) (any, error) {
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
func mcpSendMessage(cfg config.Config, in mcpSend, project string) (any, error) {
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
	a := sendArgs{to: in.To, body: in.Body, replyTo: in.ReplyTo, chat: in.Chat, project: project, files: in.Attachments, askSeats: in.AskSeats}
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
	return map[string]string{"id": m.ID, "chat": m.ChatID}, nil
}

// addTool registers a tool whose answer is the JSON of f's value as text.
func addTool[In any](s *mcp.Server, activity *mcpLifecycle, name, desc string, f func(In) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(_ context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		if activity != nil {
			if !activity.begin() {
				return nil, nil, errors.New(mcpRestartMessage)
			}
		}
		v, err := f(in)
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
