// Command agentlink is the one agentlink executable. Without a command (a
// double-click, the autostart entry, or only flags such as -config/-no-tray)
// it is the desktop app: a tray icon that runs the node in-process, answers
// requests with a local agent, and opens the settings and inbox pages in the
// browser. With a command (serve, send, wait, ...) it is the CLI that runs a
// plain node or talks to a node's local control API.
//
// It is a console program, so a terminal waits for a command and sees its
// output and exit code; the desktop app leaves the console (console_windows.go).
//
//	go build -o bin/agentlink.exe ./cmd/agentlink
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/app"
	"github.com/UberMorgott/agent-link/internal/codexqueue"
	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
)

const usage = `usage:
  agentlink [-config <settings.json>] [-no-tray] [-api <addr>]   (no command: the desktop app with its tray icon)
  agentlink serve --config <path>
  agentlink send  --config <path> [--to <node|area:NAME>] [--area <name>] --body <text> [--reply-to <id>] [--ask <node,...>] [--project <id>]   (into the one open chat with them; no --to: the only peer; the area defaults to this folder's project)
  agentlink send  --config <path> --chat <id> --body <text> [--ask <node,...>] [--ask-seat <label>] [--reply-to <id>] [--project <id>]   (to every chat participant; --ask: who must answer; --ask-seat: a local agent of this node, repeatable)
  agentlink wait  --config <path> [--timeout 0] [--chat <id>] [--project <id>]   (seconds or duration; 0 = forever; exit 2 on timeout; --chat: only that chat)
  agentlink chat new     --config <path> --with <node,...> [--area <name>] [--project <id>]   (prints the chat id; you are added; in a project: its one active chat)
  agentlink chat archive --config <path> [--chat <id>] [--project <id>]   (the project's chat history goes to the archive; a fresh chat with the same members opens; prints its id)
  agentlink chat list    --config <path> [--archive] [--legacy] [--project <id>]   (one JSON line per chat; without a project: every project's)
  agentlink chat history --config <path> --chat <id> [--limit 50] [--before <seq>] [--after <seq>] [--project <id>]   (one JSON line per message, oldest first)
  agentlink chat unread  --config <path> [--folder <path>] [--limit 50] [--after <cursor>] [--project <id>]   (unread messages for this node, oldest first, one JSON line each; a last line {"next":...} when more follow)
  agentlink chat ack     --config <path> [--chat <id>] --ids <id,...> [--session <id>] [--project <id>]   (mark read: the authors get read receipts)
  agentlink close   (no longer here: only people close chats, in the app)
  agentlink inbox --config <path> [--limit 50] [--project <id>]
  agentlink members --config <path> [--project <id>]   (one JSON line per member, this node first)
  agentlink seats [--project <id>]   (one JSON line per local agent (seat) of this node in the project; send --ask-seat <label> asks one)
  agentlink projects [--config <path>]   (one JSON line per project of the desktop app; online/total count the other members, members lists this node too)
  agentlink mcp [--config <path>]   (stdio MCP server "agentlink" for an agent session: tools projects, members, seats, chats, history, unread, send, ack)
  agentlink add    --config <path> --addr <ip[:port]> [--project <id>]   (dial a member's address; it spreads to all members)
  agentlink remove --config <path> --name <node> [--project <id>]   (remove a member from the whole network)
  agentlink hook <claude|codex> [--event auto]   (run by an agent's hooks: tells the session about new messages; never claims them)
  agentlink hook install <claude|codex> [--scope user|project]   (add that hook to ~/.claude/settings.json or ~/.codex/hooks.json)
  agentlink update [--check]    (install the latest GitHub release next to this program; --check only reports)
  agentlink version
Client commands (all but serve) may omit --config: they then use $AGENTLINK_API, else the desktop
app's settings (api, default 127.0.0.1:7520). --project (default $AGENTLINK_PROJECT_ID, set for a
project's agents) picks the project; without it the chat or message named, else this folder's
project, else the network from before projects, else the only project; failing that the error\nnames the known projects. wait exits 2 on timeout with a note on stderr.`

// exitTimeout is returned by wait when no message arrived in time.
const exitTimeout = 2

func main() {
	args := os.Args[1:]
	if appMode(args) {
		if err := runApp(args); err != nil {
			fatal(err)
		}
		return
	}
	os.Exit(run(args, os.Stdout, os.Stderr))
}

// appMode reports whether args start the desktop app: none at all, or only
// its flags. A first argument that is a word is a CLI command.
func appMode(args []string) bool {
	return len(args) == 0 || strings.HasPrefix(args[0], "-")
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	name, rest := args[0], args[1:]
	if name == "hook" {
		return runHook(rest, os.Stdin, stdout, stderr)
	}
	if name == "chat" {
		if len(rest) == 0 {
			_, _ = fmt.Fprintln(stderr, usage)
			return 1
		}
		name, rest = "chat "+rest[0], rest[1:]
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch name {
	case "version":
		_, _ = fmt.Fprintln(stdout, selfupdate.Version)
		return 0
	case "update":
		check := fs.Bool("check", false, "only report whether a newer release exists")
		if err := fs.Parse(rest); err != nil {
			return 1
		}
		if err := update(*check, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, "agentlink:", err)
			return 1
		}
		return 0
	}
	cfgPath := fs.String("config", "", "config file")
	project := new(string)
	switch name {
	case "send", "wait", "chat new", "chat archive", "chat list", "chat history", "chat unread", "chat ack", "inbox", "members", "seats", "add", "remove":
		project = fs.String("project", "", "project id (or legacy); default $"+envProjectID+", else the chat's or this folder's project")
	}
	// proj is the project selector: --project, else the agent's own project.
	proj := func() string { return cmp.Or(*project, os.Getenv(envProjectID)) }
	var cmd func(config.Config) (int, error)
	switch name {
	case "serve":
		cmd = func(c config.Config) (int, error) { return 0, serve(c) }
	case "send":
		to := fs.String("to", "", "node name or area:NAME; empty sends to the only known peer (an error listing them when there are several)")
		body := fs.String("body", "", "message text")
		replyTo := fs.String("reply-to", "", "id of the message being answered (in a chat only a reference)")
		chat := fs.String("chat", "", "chat id; default $"+envChatID+" when --to is empty")
		ask := fs.String("ask", "", "chat participants who must answer, comma-separated; none: the message only informs")
		area := fs.String("area", "", "project (area) of the conversation; default: this folder's project")
		session := fs.String("session", "", "the sending agent session's id, which gets the replies; default: the agent's own ($"+envClaudeSession+", $"+envCodexThread+")")
		var attach, askSeat listFlag
		fs.Var(&askSeat, "ask-seat", "a local agent (seat) of this node asked to answer: its label or id, or all (repeatable)")
		fs.Var(&attach, "attach", "file to attach (repeatable): an image (png, jpeg, gif, webp), pdf or text file of at most 10 MB inside the project folder or the temp folder")
		cmd = func(c config.Config) (int, error) {
			return 0, send(c, sendArgs{to: *to, body: *body, replyTo: *replyTo, chat: *chat, ask: *ask, area: *area, project: proj(), session: *session, files: attach, askSeats: askSeat}, stdout)
		}
	case "wait":
		timeout := fs.String("timeout", "0", "seconds or Go duration; 0 waits forever")
		chat := fs.String("chat", "", "only messages of this chat; others stay for a later wait")
		cmd = func(c config.Config) (int, error) { return wait(c, *timeout, *chat, proj(), stdout, stderr) }
	case "close":
		_ = fs.String("chat", "", "chat id")
		cmd = func(config.Config) (int, error) {
			// Only people close chats: agents keep writing in the one open chat.
			return 0, errors.New("close: agents do not close chats; a person closes a chat in the agentlink app (the next message then opens a new one)")
		}
	case "chat unread":
		folder := fs.String("folder", "", "only messages for a session in this folder (default: all)")
		limit := fs.Int("limit", 50, "maximum messages")
		after := fs.String("after", "", "cursor of the last message of the previous page")
		cmd = func(c config.Config) (int, error) { return 0, chatUnread(c, *folder, *after, *limit, proj(), stdout) }
	case "chat ack":
		chat := fs.String("chat", "", "chat id (default: any chat, and plain messages)")
		ids := fs.String("ids", "", "message ids, comma-separated")
		session := fs.String("session", "", "the reading session's id")
		cmd = func(c config.Config) (int, error) { return 0, chatAck(c, *chat, *ids, *session, proj(), stdout) }
	case "chat new":
		with := fs.String("with", "", "the other participants, comma-separated")
		area := fs.String("area", "", "area (project) the participants' agents work in")
		cmd = func(c config.Config) (int, error) { return 0, chatNew(c, *with, *area, proj(), stdout) }
	case "chat archive":
		chat := fs.String("chat", "", "chat id to archive (default: the project's active chat)")
		cmd = func(c config.Config) (int, error) { return 0, chatArchive(c, *chat, proj(), stdout) }
	case "chat list":
		archive := fs.Bool("archive", false, "list the archive instead of the main list")
		legacy := fs.Bool("legacy", false, "add history from before chats as virtual chats")
		cmd = func(c config.Config) (int, error) { return 0, chatList(c, *archive, *legacy, proj(), stdout) }
	case "chat history":
		chat := fs.String("chat", "", "chat id; default $"+envChatID)
		limit := fs.Int("limit", 50, "maximum messages")
		before := fs.Uint64("before", 0, "only messages before this seq (0: up to the newest)")
		after := fs.Uint64("after", 0, "only messages after this seq, oldest first")
		cmd = func(c config.Config) (int, error) {
			return 0, chatHistory(c, *chat, *limit, *before, *after, proj(), stdout)
		}
	case "inbox":
		limit := fs.Int("limit", 50, "maximum entries")
		cmd = func(c config.Config) (int, error) { return 0, inbox(c, *limit, proj(), stdout) }
	case "members":
		cmd = func(c config.Config) (int, error) { return 0, members(c, "", nil, proj(), stdout) }
	case "seats":
		cmd = func(c config.Config) (int, error) {
			list, err := listSeats(c, proj())
			if err != nil {
				return 0, err
			}
			return 0, encodeLines(stdout, list)
		}
	case "projects":
		cmd = func(c config.Config) (int, error) { return 0, projects(c, stdout) }
	case "mcp":
		cmd = func(c config.Config) (int, error) { return 0, runMCP(c) }
	case "add":
		addr := fs.String("addr", "", "IP or host of a member, port optional")
		cmd = func(c config.Config) (int, error) {
			return 0, members(c, "/members", &node.MemberRequest{Addr: *addr}, proj(), stdout)
		}
	case "remove":
		name := fs.String("name", "", "node name of the member")
		cmd = func(c config.Config) (int, error) {
			return 0, members(c, "/members/remove", &node.MemberRequest{Name: *name}, proj(), stdout)
		}
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	if err := fs.Parse(rest); err != nil {
		return 1
	}
	cfg, err := loadConfig(name, *cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	code, err := cmd(cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "agentlink:", err)
		return 1
	}
	return code
}

// loadConfig reads the --config file. Without one, a client command only needs
// the local API address: $AGENTLINK_API (set by the worker for its agents), else
// the desktop app's settings (api key, absent: settings.DefaultAPI). serve
// always needs --config.
func loadConfig(name, path string) (config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	if name == "serve" {
		return config.Config{}, errors.New("--config is required")
	}
	if api := os.Getenv(envAPI); api != "" {
		return config.Config{API: api}, nil
	}
	p, err := settings.DefaultPath()
	if err != nil {
		return config.Config{}, err
	}
	s, _, err := settings.Load(p)
	if err != nil {
		return config.Config{}, err
	}
	return config.Config{API: s.APIAddr()}, nil
}

func serve(cfg config.Config) error {
	secret, err := cfg.Secret()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if config.WeakCode(os.Getenv(cfg.SecretEnv)) && config.ExposedListen(cfg.Listen) {
		log.Warn("legacy 6-character pairing code while the listener is reachable beyond private networks: generate a XXXX-XXXX-XXXX code", "listen", cfg.Listen)
	}
	n, err := node.New(cfg, secret, log)
	if err != nil {
		return err
	}
	n.SetAppVersion(selfupdate.Version)
	n.SetSessionWaker(codexqueue.New())
	n.SetInboxPoster(node.PipePoster{})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var lc net.ListenConfig
	peerLn, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	apiLn, err := lc.Listen(ctx, "tcp", cfg.API)
	if err != nil {
		_ = peerLn.Close()
		return err
	}
	return n.Serve(ctx, peerLn, apiLn)
}

// Environment of a job's agent, set by the worker: the node's local API, its
// project, the chat and the request the job answers. Without --config the CLI
// talks to that API and names that project; send and chat history fall back
// to the chat; a chat send passes the request on, so the node continues its
// automatic chain.
const (
	envAPI       = "AGENTLINK_API"
	envProjectID = "AGENTLINK_PROJECT_ID"
	envChatID    = "AGENTLINK_CHAT_ID"
	envJobID     = "AGENTLINK_JOB_ID"
	// envSeat is the seat whose turn the node runs (node.Seat): send writes as it.
	envSeat = "AGENTLINK_SEAT"
)

// withProject adds the project selector to q (a new one when q is nil).
func withProject(q url.Values, project string) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if project != "" {
		q.Set("project", project)
	}
	return q
}

// inFolder is withProject for a request that names no chat or message: without
// a project the working folder is the app's hint for one (the "cwd" query).
func inFolder(q url.Values, project string) url.Values {
	q = withProject(q, project)
	if project == "" {
		if wd, err := os.Getwd(); err == nil {
			q.Set("cwd", wd)
		}
	}
	return q
}

type sendArgs struct {
	to, body, replyTo, chat, ask, area, project, session string
	files                                                []string // local files to attach
	askSeats                                             []string // local agents (seats) asked
}

// listFlag is a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// Environment of the agent sessions agentlink send runs in: the session's id,
// so replies go back to that session.
const (
	envClaudeSession = "CLAUDE_CODE_SESSION_ID"
	envCodexThread   = "CODEX_THREAD_ID"
	envCodexSession  = "CODEX_SESSION_ID" // older Codex builds
)

// agentSession is the id of the agent session this command runs in, and its
// hook client; empty outside one and inside a worker's job (not a session).
func agentSession() (id, client string) {
	if os.Getenv(envJobID) != "" {
		return "", ""
	}
	if id := os.Getenv(envClaudeSession); id != "" {
		return id, hookClaude
	}
	if id := cmp.Or(os.Getenv(envCodexThread), os.Getenv(envCodexSession)); id != "" {
		return id, hookCodex
	}
	return "", ""
}

func send(cfg config.Config, a sendArgs, stdout io.Writer) error {
	if a.body == "" && len(a.files) == 0 {
		return errors.New("--body is required (or --attach)")
	}
	if a.chat == "" && a.to == "" {
		a.chat = os.Getenv(envChatID)
	}
	var ask []string
	if a.ask != "" {
		ask = []string{a.ask}
	}
	m, err := sendMessage(cfg, a, ask)
	if err != nil {
		return err
	}
	if m.ChatID != "" && a.chat == "" {
		// send --to continued the chat with that member; stdout stays the id alone.
		fmt.Fprintln(os.Stderr, "chat "+m.ChatID)
	}
	_, err = fmt.Fprintln(stdout, m.ID)
	return err
}

// sendMessage posts one message (to a.chat, else a.to) for send and the MCP
// send tool; the session defaults to the agent's own and learns the chat.
func sendMessage(cfg config.Config, a sendArgs, ask []string) (node.Message, error) {
	if a.session == "" {
		a.session, _ = agentSession()
	}
	r := node.SendRequest{To: a.to, Body: a.body, ReplyTo: a.replyTo, ChatID: a.chat, Area: a.area, SessionID: a.session, Ask: ask,
		AskSeats: a.askSeats, Seat: os.Getenv(envSeat)}
	if wd, err := os.Getwd(); err == nil {
		r.Folder = wd // the node picks the project of this folder
	}
	for _, f := range a.files {
		p, err := filepath.Abs(f) // the node resolves no relative paths of its own
		if err != nil {
			return node.Message{}, err
		}
		r.Files = append(r.Files, p)
	}
	// A job's agent names its request: in a chat that continues its chain, and
	// its own reply to it never counts as answered by someone else.
	r.Parent = os.Getenv(envJobID)
	var m node.Message
	if err := apiJSON(http.MethodPost, apiURL(cfg, "/send", withProject(nil, a.project)), r, &m); err != nil {
		return node.Message{}, err
	}
	if m.ChatID != "" && r.SessionID != "" {
		if p, err := settings.DefaultPath(); err == nil {
			noteSent(hookEnv{api: cfg.API, dir: filepath.Join(filepath.Dir(p), "hooks")}, r.SessionID, m.ChatID, m.ID)
		}
	}
	return m, nil
}

func wait(cfg config.Config, timeout, chat, project string, stdout, stderr io.Writer) (int, error) {
	d, err := parseTimeout(timeout)
	if err != nil {
		return 1, err
	}
	q := withProject(url.Values{"timeout": {d.String()}}, project)
	if chat != "" {
		q.Set("chat", chat)
	}
	if wd, err := os.Getwd(); err == nil {
		q.Set("folder", wd) // the app picks the project of this folder
	}
	resp, err := apiDo(http.MethodGet, apiURL(cfg, "/wait", q), nil)
	if err != nil {
		return 1, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		// stdout stays empty; the exit code and this line tell a timeout from an error.
		_, _ = fmt.Fprintf(stderr, "agentlink: no message within %s\n", d)
		return exitTimeout, nil
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return 1, err
	}
	return 0, printLines[node.Message](resp.Body, stdout)
}

func inbox(cfg config.Config, limit int, project string, stdout io.Writer) error {
	resp, err := apiDo(http.MethodGet, apiURL(cfg, "/inbox", inFolder(url.Values{"limit": {strconv.Itoa(limit)}}, project)), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	return printLines[node.Entry](resp.Body, stdout)
}

func chatNew(cfg config.Config, with, area, project string, stdout io.Writer) error {
	if strings.TrimSpace(with) == "" {
		return errors.New("--with is required")
	}
	info, err := createChat(cfg, []string{with}, area, project)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, info.ID)
	return err
}

// createChat opens a chat with the participants (names, comma lists allowed).
func createChat(cfg config.Config, with []string, area, project string) (node.ChatInfo, error) {
	var info node.ChatInfo
	err := apiJSON(http.MethodPost, apiURL(cfg, "/chats", inFolder(nil, project)), node.CreateChatRequest{Participants: with, Area: area}, &info)
	return info, err
}

// chatArchive moves the project's chat history to the archive and prints the
// id of the fresh active chat.
func chatArchive(cfg config.Config, chat, project string, stdout io.Writer) error {
	var info node.ChatInfo
	if err := apiJSON(http.MethodPost, apiURL(cfg, "/chats/archive", inFolder(nil, project)), node.ArchiveRequest{ChatID: chat}, &info); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stdout, info.ID)
	return err
}

func chatList(cfg config.Config, archive, legacy bool, project string, stdout io.Writer) error {
	chats, err := listChats(cfg, archive, legacy, project)
	if err != nil {
		return err
	}
	return encodeLines(stdout, chats)
}

func listChats(cfg config.Config, archive, legacy bool, project string) ([]node.ChatInfo, error) {
	q := withProject(nil, project)
	if archive {
		q.Set("archive", "1")
	}
	if legacy {
		q.Set("legacy", "1")
	}
	var chats []node.ChatInfo
	err := apiJSON(http.MethodGet, apiURL(cfg, "/chats", q), nil, &chats)
	return chats, err
}

func chatHistory(cfg config.Config, chat string, limit int, before, after uint64, project string, stdout io.Writer) error {
	if chat == "" {
		chat = os.Getenv(envChatID)
	}
	if chat == "" {
		return errors.New("--chat is required")
	}
	msgs, err := history(cfg, chat, limit, before, after, project)
	if err != nil {
		return err
	}
	return encodeLines(stdout, msgs)
}

func history(cfg config.Config, chat string, limit int, before, after uint64, project string) ([]node.ChatMessage, error) {
	q := withProject(url.Values{"limit": {strconv.Itoa(limit)}}, project)
	if before > 0 {
		q.Set("before", strconv.FormatUint(before, 10))
	}
	if after > 0 {
		q.Set("after", strconv.FormatUint(after, 10))
	}
	var msgs []node.ChatMessage
	err := apiJSON(http.MethodGet, apiURL(cfg, "/chats/"+url.PathEscape(chat)+"/messages", q), nil, &msgs)
	return msgs, err
}

// chatUnread prints this node's unread messages, one JSON line each, and a
// last line {"next": cursor, "total": n} when more pages follow.
func chatUnread(cfg config.Config, folder, after string, limit int, project string, stdout io.Writer) error {
	page, err := unread(cfg, folder, after, limit, project)
	if err != nil {
		return err
	}
	if err := encodeLines(stdout, page.Messages); err != nil {
		return err
	}
	if page.Next == "" {
		return nil
	}
	return encodeLines(stdout, []map[string]any{{"next": page.Next, "total": page.Total}})
}

// unread reads one page of unread messages; it never marks them read.
func unread(cfg config.Config, folder, after string, limit int, project string) (node.UnreadPage, error) {
	q := inFolder(url.Values{"limit": {strconv.Itoa(limit)}}, project)
	if folder != "" {
		abs, err := filepath.Abs(folder)
		if err != nil {
			return node.UnreadPage{}, err
		}
		q.Set("folder", abs)
	}
	if after != "" {
		q.Set("after", after)
	}
	var page node.UnreadPage
	err := apiJSON(http.MethodGet, apiURL(cfg, "/unread", q), nil, &page)
	return page, err
}

// chatAck marks messages read and prints one JSON line per id (node.AckResult).
func chatAck(cfg config.Config, chat, ids, session, project string, stdout io.Writer) error {
	var list []string
	for id := range strings.SplitSeq(ids, ",") {
		if id = strings.TrimSpace(id); id != "" {
			list = append(list, id)
		}
	}
	if len(list) == 0 {
		return errors.New("--ids is required")
	}
	res, err := ack(cfg, chat, list, session, project)
	if err != nil {
		return err
	}
	return encodeLines(stdout, res)
}

// ack marks ids read, in chat when named, for the reading session.
func ack(cfg config.Config, chat string, ids []string, session, project string) ([]node.AckResult, error) {
	path := "/ack"
	if chat != "" {
		path = "/chats/" + url.PathEscape(chat) + "/ack"
	}
	var res []node.AckResult
	err := apiJSON(http.MethodPost, apiURL(cfg, path, withProject(nil, project)), node.AckRequest{IDs: ids, SessionID: session}, &res)
	return res, err
}

// projects prints the projects of the desktop app, one JSON line each.
func projects(cfg config.Config, stdout io.Writer) error {
	list, err := listProjects(cfg)
	if err != nil {
		return err
	}
	return encodeLines(stdout, list)
}

func listProjects(cfg config.Config) ([]app.ProjectView, error) {
	var list []app.ProjectView
	err := apiJSON(http.MethodGet, apiURL(cfg, "/projects", nil), nil, &list)
	return list, err
}

// apiJSON calls the local API with req as the JSON body (nil: none) and
// decodes a 200 answer into out.
func apiJSON(method, u string, req, out any) error {
	var body []byte
	if req != nil {
		var err error
		if body, err = json.Marshal(req); err != nil {
			return err
		}
	}
	resp, err := apiDo(method, u, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// members lists the member table, after posting req to path when req is set.
func members(cfg config.Config, path string, req *node.MemberRequest, project string, stdout io.Writer) error {
	var list []node.MemberInfo
	var err error
	if req == nil {
		list, err = listMembers(cfg, project)
	} else {
		if req.Addr == "" && req.Name == "" {
			return errors.New("--addr or --name is required")
		}
		err = apiJSON(http.MethodPost, apiURL(cfg, path, inFolder(nil, project)), req, &list)
	}
	if err != nil {
		return err
	}
	return encodeLines(stdout, list)
}

// listSeats lists the local agents (seats) of the project's node.
func listSeats(cfg config.Config, project string) ([]node.SeatView, error) {
	var list []node.SeatView
	err := apiJSON(http.MethodGet, apiURL(cfg, "/seats", inFolder(nil, project)), nil, &list)
	return list, err
}

func listMembers(cfg config.Config, project string) ([]node.MemberInfo, error) {
	var list []node.MemberInfo
	err := apiJSON(http.MethodGet, apiURL(cfg, "/members", inFolder(nil, project)), nil, &list)
	return list, err
}

// update reports the latest release and, unless check, installs it over this
// program. A running desktop app keeps the old version until it restarts.
func update(check bool, stdout io.Writer) error {
	cur := selfupdate.Version
	if !selfupdate.Valid(cur) {
		return fmt.Errorf("this build has no version (%s): self-update is off; use a release build", cur)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_ = selfupdate.Cleanup(exe) // leftovers of an earlier update, if no longer locked
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rel, newer, err := selfupdate.Check(ctx, cur)
	switch {
	case err != nil:
		return err
	case rel == nil:
		_, err = fmt.Fprintf(stdout, "agentlink %s: no release for this platform on github.com/%s\n", cur, selfupdate.Repo)
		return err
	case !newer:
		_, err = fmt.Fprintf(stdout, "agentlink %s is up to date (latest release %s)\n", cur, rel.Version())
		return err
	case check:
		_, err = fmt.Fprintf(stdout, "agentlink %s: update available: %s (run: agentlink update)\n", cur, rel.Version())
		return err
	}
	paths, err := rel.Apply(ctx, exe)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := fmt.Fprintf(stdout, "updated %s\n", p); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "agentlink %s -> %s; restart the agentlink app if it is running\n", cur, rel.Version())
	return err
}

// parseTimeout accepts whole seconds ("30") or a Go duration ("1m30s").
func parseTimeout(s string) (time.Duration, error) {
	if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid --timeout %q", s)
	}
	return d, nil
}

// apiDo sends one request to the local API: a POST carries body as JSON.
func apiDo(method, u string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func apiURL(cfg config.Config, path string, q url.Values) string {
	u := url.URL{Scheme: "http", Host: cfg.API, Path: path, RawQuery: q.Encode()}
	return u.String()
}

func checkStatus(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &apiError{status: resp.Status, msg: string(bytes.TrimSpace(msg))}
}

// apiError is a local API answer other than the expected status; msg is the
// API's own error text.
type apiError struct{ status, msg string }

func (e *apiError) Error() string { return "api " + e.status + ": " + e.msg }

// printLines decodes a JSON array and prints one compact JSON object per line.
func printLines[T any](r io.Reader, w io.Writer) error {
	var items []T
	if err := json.NewDecoder(r).Decode(&items); err != nil {
		return err
	}
	return encodeLines(w, items)
}

// encodeLines prints one compact JSON object per line.
func encodeLines[T any](w io.Writer, items []T) error {
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}
