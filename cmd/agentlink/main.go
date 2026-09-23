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
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
)

const usage = `usage:
  agentlink [-config <settings.json>] [-no-tray] [-api <addr>]   (no command: the desktop app with its tray icon)
  agentlink serve --config <path>
  agentlink send  --config <path> [--to <node|area:NAME>] --body <text> [--reply-to <id>]   (no --to: the only peer; with several it fails and lists them)
  agentlink send  --config <path> --chat <id> --body <text> [--ask <node,...>] [--reply-to <id>]   (to every chat participant; --ask: who must answer)
  agentlink wait  --config <path> [--timeout 0] [--chat <id>]   (seconds or duration; 0 = forever; exit 2 on timeout; --chat: only that chat)
  agentlink chat new     --config <path> --with <node,...> [--area <name>]   (prints the chat id; you are added)
  agentlink chat list    --config <path> [--archive] [--legacy]   (one JSON line per chat)
  agentlink chat history --config <path> --chat <id> [--limit 50] [--before <seq>] [--after <seq>]   (one JSON line per message, oldest first)
  agentlink chat archive --config <path> --chat <id> [--undo]
  agentlink close --config <path> --chat <id>      (close the chat for every participant)
  agentlink inbox --config <path> [--limit 50]
  agentlink members --config <path>                (one JSON line per member, this node first)
  agentlink add    --config <path> --addr <ip[:port]>   (dial a member's address; it spreads to all members)
  agentlink remove --config <path> --name <node>        (remove a member from the whole network)
  agentlink update [--check]    (install the latest GitHub release next to this program; --check only reports)
  agentlink version`

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
		cmd = func(c config.Config) (int, error) {
			return 0, send(c, sendArgs{to: *to, body: *body, replyTo: *replyTo, chat: *chat, ask: *ask}, stdout)
		}
	case "wait":
		timeout := fs.String("timeout", "0", "seconds or Go duration; 0 waits forever")
		chat := fs.String("chat", "", "only messages of this chat; others stay for a later wait")
		cmd = func(c config.Config) (int, error) { return wait(c, *timeout, *chat, stdout) }
	case "close":
		chat := fs.String("chat", "", "chat id")
		cmd = func(c config.Config) (int, error) { return 0, chatPost(c, *chat, "/close", nil, stdout) }
	case "chat new":
		with := fs.String("with", "", "the other participants, comma-separated")
		area := fs.String("area", "", "area (project) the participants' agents work in")
		cmd = func(c config.Config) (int, error) { return 0, chatNew(c, *with, *area, stdout) }
	case "chat list":
		archive := fs.Bool("archive", false, "list the archive instead of the main list")
		legacy := fs.Bool("legacy", false, "add history from before chats as virtual chats")
		cmd = func(c config.Config) (int, error) { return 0, chatList(c, *archive, *legacy, stdout) }
	case "chat history":
		chat := fs.String("chat", "", "chat id; default $"+envChatID)
		limit := fs.Int("limit", 50, "maximum messages")
		before := fs.Uint64("before", 0, "only messages before this seq (0: up to the newest)")
		after := fs.Uint64("after", 0, "only messages after this seq, oldest first")
		cmd = func(c config.Config) (int, error) { return 0, chatHistory(c, *chat, *limit, *before, *after, stdout) }
	case "chat archive":
		chat := fs.String("chat", "", "chat id")
		undo := fs.Bool("undo", false, "bring the chat back to the main list")
		cmd = func(c config.Config) (int, error) {
			return 0, chatPost(c, *chat, "/archive", node.ArchiveRequest{Archived: !*undo}, stdout)
		}
	case "inbox":
		limit := fs.Int("limit", 50, "maximum entries")
		cmd = func(c config.Config) (int, error) { return 0, inbox(c, *limit, stdout) }
	case "members":
		cmd = func(c config.Config) (int, error) { return 0, members(c, "", nil, stdout) }
	case "add":
		addr := fs.String("addr", "", "IP or host of a member, port optional")
		cmd = func(c config.Config) (int, error) {
			return 0, members(c, "/members", &node.MemberRequest{Addr: *addr}, stdout)
		}
	case "remove":
		name := fs.String("name", "", "node name of the member")
		cmd = func(c config.Config) (int, error) {
			return 0, members(c, "/members/remove", &node.MemberRequest{Name: *name}, stdout)
		}
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	if err := fs.Parse(rest); err != nil {
		return 1
	}
	if *cfgPath == "" {
		_, _ = fmt.Fprintln(stderr, "--config is required")
		return 1
	}
	cfg, err := config.Load(*cfgPath)
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

// Environment of a job's agent, set by the worker: the chat and the request
// the job answers. send and chat history fall back to the chat; a chat send
// passes the request on, so the node continues its automatic chain.
const (
	envChatID = "AGENTLINK_CHAT_ID"
	envJobID  = "AGENTLINK_JOB_ID"
)

type sendArgs struct{ to, body, replyTo, chat, ask string }

func send(cfg config.Config, a sendArgs, stdout io.Writer) error {
	if a.body == "" {
		return errors.New("--body is required")
	}
	if a.chat == "" && a.to == "" {
		a.chat = os.Getenv(envChatID)
	}
	r := node.SendRequest{To: a.to, Body: a.body, ReplyTo: a.replyTo, ChatID: a.chat}
	if a.ask != "" {
		r.Ask = []string{a.ask}
	}
	if a.chat != "" {
		r.Parent = os.Getenv(envJobID)
	}
	req, err := json.Marshal(r)
	if err != nil {
		return err
	}
	resp, err := apiDo(http.MethodPost, apiURL(cfg, "/send", nil), req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	var m node.Message
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, m.ID)
	return err
}

func wait(cfg config.Config, timeout, chat string, stdout io.Writer) (int, error) {
	d, err := parseTimeout(timeout)
	if err != nil {
		return 1, err
	}
	q := url.Values{"timeout": {d.String()}}
	if chat != "" {
		q.Set("chat", chat)
	}
	resp, err := apiDo(http.MethodGet, apiURL(cfg, "/wait", q), nil)
	if err != nil {
		return 1, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return exitTimeout, nil
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return 1, err
	}
	return 0, printLines[node.Message](resp.Body, stdout)
}

func inbox(cfg config.Config, limit int, stdout io.Writer) error {
	resp, err := apiDo(http.MethodGet, apiURL(cfg, "/inbox", url.Values{"limit": {strconv.Itoa(limit)}}), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	return printLines[node.Entry](resp.Body, stdout)
}

func chatNew(cfg config.Config, with, area string, stdout io.Writer) error {
	if strings.TrimSpace(with) == "" {
		return errors.New("--with is required")
	}
	var info node.ChatInfo
	if err := apiJSON(http.MethodPost, apiURL(cfg, "/chats", nil), node.CreateChatRequest{Participants: []string{with}, Area: area}, &info); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stdout, info.ID)
	return err
}

func chatList(cfg config.Config, archive, legacy bool, stdout io.Writer) error {
	q := url.Values{}
	if archive {
		q.Set("archive", "1")
	}
	if legacy {
		q.Set("legacy", "1")
	}
	var chats []node.ChatInfo
	if err := apiJSON(http.MethodGet, apiURL(cfg, "/chats", q), nil, &chats); err != nil {
		return err
	}
	return encodeLines(stdout, chats)
}

func chatHistory(cfg config.Config, chat string, limit int, before, after uint64, stdout io.Writer) error {
	if chat == "" {
		chat = os.Getenv(envChatID)
	}
	if chat == "" {
		return errors.New("--chat is required")
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before > 0 {
		q.Set("before", strconv.FormatUint(before, 10))
	}
	if after > 0 {
		q.Set("after", strconv.FormatUint(after, 10))
	}
	var msgs []node.ChatMessage
	if err := apiJSON(http.MethodGet, apiURL(cfg, "/chats/"+url.PathEscape(chat)+"/messages", q), nil, &msgs); err != nil {
		return err
	}
	return encodeLines(stdout, msgs)
}

// chatPost posts body (nil: none) to /chats/{chat}<action> and prints the chat as one JSON line.
func chatPost(cfg config.Config, chat, action string, body any, stdout io.Writer) error {
	if chat == "" {
		return errors.New("--chat is required")
	}
	var info node.ChatInfo
	if err := apiJSON(http.MethodPost, apiURL(cfg, "/chats/"+url.PathEscape(chat)+action, nil), body, &info); err != nil {
		return err
	}
	return encodeLines(stdout, []node.ChatInfo{info})
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
func members(cfg config.Config, path string, req *node.MemberRequest, stdout io.Writer) error {
	var resp *http.Response
	var err error
	if req == nil {
		resp, err = apiDo(http.MethodGet, apiURL(cfg, "/members", nil), nil)
	} else {
		if req.Addr == "" && req.Name == "" {
			return errors.New("--addr or --name is required")
		}
		body, merr := json.Marshal(req)
		if merr != nil {
			return merr
		}
		resp, err = apiDo(http.MethodPost, apiURL(cfg, path, nil), body)
	}
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	return printLines[node.MemberInfo](resp.Body, stdout)
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
	return fmt.Errorf("api %s: %s", resp.Status, bytes.TrimSpace(msg))
}

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
