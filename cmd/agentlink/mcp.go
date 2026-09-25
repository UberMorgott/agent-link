package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
		Body        string   `json:"body" jsonschema:"message text"`
		Ask         []string `json:"ask,omitempty" jsonschema:"participants who must answer; none: the message only informs"`
		ReplyTo     string   `json:"reply_to,omitempty" jsonschema:"id of the message being answered"`
	}
	mcpAck struct {
		IDs     []string `json:"ids" jsonschema:"message ids to mark read"`
		Chat    string   `json:"chat,omitempty" jsonschema:"chat id (default: any chat, and plain messages)"`
		Project string   `json:"project,omitempty" jsonschema:"project id (or legacy); default: $AGENTLINK_PROJECT_ID"`
		Session string   `json:"session,omitempty" jsonschema:"the reading session's id; default: this agent session"`
	}
)

// runMCP serves the MCP tools on stdin/stdout until the client leaves.
func runMCP(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return newMCPServer(cfg).Run(ctx, &mcp.StdioTransport{})
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
		return unread(cfg, in.Folder, in.After, limit(in.Limit), proj(in.Project))
	})
	addTool(s, "send", "Send a message: into a chat (chat), the open chat with a member (to), or a new chat (new_chat_with). Returns {id, chat}.", func(in mcpSend) (any, error) {
		return mcpSendMessage(cfg, in, proj(""))
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
	if in.Body == "" {
		return nil, errors.New("body is required")
	}
	a := sendArgs{to: in.To, body: in.Body, replyTo: in.ReplyTo, chat: in.Chat, project: project}
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
func addTool[In any](s *mcp.Server, name, desc string, f func(In) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(_ context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
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
