package main

// agentlink hook: tells an interactive Claude Code or Codex session about
// messages that arrived at this node, through the agent's own hooks. Nobody can
// type into a live session on another machine, but its SessionStart,
// UserPromptSubmit, PostToolUse and Stop hooks run this command, and what it
// prints becomes context for the model (or, on Stop, a reason to keep going).
//
// The hook only reads: it lists chats and the inbox and never claims messages,
// so a background `agentlink wait` still gets every one of them. What a
// session was already shown is kept in a per-session cursor file.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Hook clients.
const (
	hookClaude = agenthook.Claude
	hookCodex  = agenthook.Codex
)

// Hook events this command answers; others are ignored.
const (
	evSessionStart = agenthook.SessionStart
	evPrompt       = agenthook.Prompt
	evPostTool     = agenthook.PostTool
	evStop         = agenthook.Stop
)

// hookEvents are installed by `agentlink hook install`, in this order.
var hookEvents = agenthook.Events

// Limits of what one hook prints.
const (
	hookMaxBody     = 4000 // runes of one message body
	hookMaxMessages = 10   // messages listed in full; the rest only counted
	hookHTTPTimeout = 3 * time.Second
	hookCursorTTL   = 14 * 24 * time.Hour // cursor files older than this are removed
	hookFirstWindow = 24 * time.Hour      // a new session hears of pending messages this recent
)

// hookInput is the part of the hook's stdin JSON this command reads. Claude
// Code and Codex send the same fields.
type hookInput struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
	Cwd            string `json:"cwd"`
}

// hookCursor is what one session has already been shown: the last chat Seq
// per chat and the ids of inbox messages outside chats.
type hookCursor struct {
	Chats map[string]uint64 `json:"chats"`
	Inbox []string          `json:"inbox"`
}

// hookMessage is one message the session is told about.
type hookMessage struct {
	ID           string
	From         string
	ChatID       string
	Participants []string
	At           time.Time
	Body         string
	AsksMe       bool
}

// hookEnv is where the hook finds the node and keeps its cursors.
type hookEnv struct {
	api string // host:port of the node's local API
	dir string // cursor directory
	// projects maps an area to its project folder (Settings.Projects).
	projects map[string]string
}

// runHook runs `agentlink hook ...`. A hook never breaks the session it runs
// in: every failure (no node, bad input) ends with exit 0 and no output.
func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "install" {
		return runHookInstall(args[1:], stdout, stderr)
	}
	if len(args) == 0 || (args[0] != hookClaude && args[0] != hookCodex) {
		_, _ = fmt.Fprintln(stderr, "usage: agentlink hook <claude|codex> [--event auto] | agentlink hook install <claude|codex> [--scope user|project]")
		return 1
	}
	client := args[0]
	flags := flag.NewFlagSet("hook", flag.ContinueOnError)
	flags.SetOutput(stderr)
	event := flags.String("event", "auto", "hook event; auto: hook_event_name of the input")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	// An agent the worker started for a job already gets its request; telling it
	// about the chat's messages again would only loop.
	if os.Getenv(envJobID) != "" {
		return 0
	}
	env, err := defaultHookEnv()
	if err != nil {
		return 0
	}
	// On failure out is still what the event needs when there is no news.
	out, _ := hookRun(client, *event, stdin, env)
	if out == "" {
		return 0
	}
	_, _ = fmt.Fprintln(stdout, out)
	return 0
}

// defaultHookEnv finds the node API like the other client commands and keeps
// cursors in %APPDATA%\agentlink\hooks.
func defaultHookEnv() (hookEnv, error) {
	cfg, err := loadConfig("hook", "")
	if err != nil {
		return hookEnv{}, err
	}
	p, err := settings.DefaultPath()
	if err != nil {
		return hookEnv{}, err
	}
	env := hookEnv{api: cfg.API, dir: filepath.Join(filepath.Dir(p), "hooks")}
	if s, _, err := settings.Load(p); err == nil {
		env.projects = map[string]string{}
		for area, pr := range s.Projects {
			env.projects[area] = pr.Dir
		}
	}
	return env, nil
}

// hookRun reads the hook input, collects the messages this session has not
// seen and returns what to print (empty: nothing).
func hookRun(client, event string, stdin io.Reader, env hookEnv) (string, error) {
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return "", err
	}
	if event == "" || event == "auto" {
		event = in.HookEventName
	}
	if !slices.Contains(hookEvents, event) || in.SessionID == "" {
		return "", nil
	}
	quiet := ""
	if client == hookCodex && event == evStop {
		quiet = "{}" // Codex wants JSON on stdout from a Stop hook that exits 0
	}
	path := filepath.Join(env.dir, client+"-"+sessionFileName(in.SessionID)+".json")
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return quiet, err
	}
	defer unlock()
	if event == evSessionStart {
		pruneCursors(env.dir)
	}
	cur, first, err := loadCursor(path)
	if err != nil {
		return quiet, err
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	msgs, err := collectHookMessages(env.api, &cur, first, hookFilter(cwd, env.projects))
	if err != nil {
		return quiet, err
	}
	if err := writeFileAtomic(path, mustJSON(cur)); err != nil {
		return quiet, err
	}
	if len(msgs) == 0 {
		return quiet, nil
	}
	return hookOutput(event, formatHookMessages(event, msgs)), nil
}

// hookOutput wraps text in the event's JSON output: extra context for the
// model, or on Stop a block whose reason makes the agent continue with it.
// Claude Code and Codex share these shapes.
func hookOutput(event, text string) string {
	var v any
	if event == evStop {
		v = map[string]string{"decision": "block", "reason": text}
	} else {
		v = map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": text}}
	}
	return strings.TrimSpace(string(mustJSON(v)))
}

// collectHookMessages lists the messages newer than cur and moves cur past
// them. On the first call of a session (first) it only records where chats
// are now and reports inbox messages of the last day that no `wait` took and
// this node has not answered. Messages of an area wants refuses are skipped.
func collectHookMessages(api string, cur *hookCursor, first bool, wants func(area string) bool) ([]hookMessage, error) {
	var out []hookMessage
	var chats []node.ChatInfo
	if err := hookGet(api, "/chats", nil, &chats); err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	seenChats := map[string]uint64{}
	for _, c := range chats {
		last, known := cur.Chats[c.ID]
		seenChats[c.ID] = max(last, c.LastSeq)
		if first || c.LastSeq <= last || !wants(c.Area) {
			continue
		}
		self := ""
		for _, m := range c.Members {
			if m.Self {
				self = m.Name
			}
		}
		q := url.Values{"limit": {"200"}}
		if known && last > 0 {
			q.Set("after", strconv.FormatUint(last, 10))
		}
		var msgs []node.ChatMessage
		if err := hookGet(api, "/chats/"+url.PathEscape(c.ID)+"/messages", q, &msgs); errors.Is(err, errNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.Seq <= last {
				continue
			}
			if m.Direction == "in" && m.Kind == "" && m.Body != "" {
				out = append(out, hookMessage{
					ID: m.ID, From: m.From, ChatID: c.ID, Participants: c.Participants,
					At: m.CreatedAt, Body: m.Body, AsksMe: self != "" && m.Asks(self),
				})
			}
		}
		if n := len(msgs); n > 0 {
			seenChats[c.ID] = max(last, msgs[n-1].Seq)
		}
	}
	// Chats no longer in the main list (archived) keep their place.
	for id, seq := range cur.Chats {
		if _, ok := seenChats[id]; !ok {
			seenChats[id] = seq
		}
	}
	cur.Chats = seenChats

	var entries []node.Entry
	if err := hookGet(api, "/inbox", url.Values{"limit": {"100"}}, &entries); err != nil {
		return nil, err
	}
	answered := map[string]bool{} // requests this node already replied to
	for _, e := range entries {
		if e.Direction == "out" && e.ReplyTo != "" {
			answered[e.ReplyTo] = true
		}
	}
	var inbox []string
	for _, e := range slices.Backward(entries) { // oldest first
		if e.Direction != "in" || e.Kind != "" || e.ChatID != "" {
			continue
		}
		inbox = append(inbox, e.ID)
		if slices.Contains(cur.Inbox, e.ID) || !wants(e.Area) {
			continue
		}
		// A new session hears only of recent messages nobody took or answered yet.
		if first && (e.Status != "pending" || answered[e.ID] || time.Since(e.CreatedAt) > hookFirstWindow) {
			continue
		}
		out = append(out, hookMessage{ID: e.ID, From: e.From, At: e.CreatedAt, Body: e.Body})
	}
	// The inbox lists the newest entries, so ids older than those never come back.
	cur.Inbox = inbox
	slices.SortStableFunc(out, func(a, b hookMessage) int { return a.At.Compare(b.At) })
	return out, nil
}

// hookFilter decides which messages a session in folder cwd hears about. A
// session inside the project folder of an area (the deepest one when folders
// nest) gets only that area's messages: its chats and area:NAME messages. Any
// other session (the working folder, a user-scope hook elsewhere) gets the
// rest: direct messages, chats without an area and areas with no project
// folder. That is where the worker answers them too.
func hookFilter(cwd string, projects map[string]string) func(area string) bool {
	var mine []string
	best := -1
	for area, dir := range projects {
		if dir == "" || !inFolder(dir, cwd) {
			continue
		}
		switch n := len(filepath.Clean(dir)); {
		case n > best:
			best, mine = n, []string{area}
		case n == best:
			mine = append(mine, area)
		}
	}
	if mine != nil {
		return func(area string) bool { return slices.Contains(mine, area) }
	}
	return func(area string) bool { return area == "" || projects[area] == "" }
}

// inFolder reports whether path is dir or inside it (case-insensitively on
// Windows, like filepath.Rel).
func inFolder(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// formatHookMessages is the text the model reads: every new message with its
// sender, chat and the command that answers it.
func formatHookMessages(event string, msgs []hookMessage) string {
	var b strings.Builder
	if event == evStop {
		b.WriteString("Пока вы работали, пришли новые сообщения agentlink. Прочитайте их и, если нужно, ответьте, затем завершайте.\n")
	} else {
		fmt.Fprintf(&b, "agentlink: новые сообщения (%d).\n", len(msgs))
	}
	shown := msgs
	if len(shown) > hookMaxMessages {
		shown = shown[len(shown)-hookMaxMessages:]
	}
	for _, m := range shown {
		b.WriteString("\n")
		if m.ChatID != "" {
			fmt.Fprintf(&b, "Пришло сообщение от %s в чате %s (участники: %s), %s", m.From, m.ChatID, strings.Join(m.Participants, ", "), m.At.Local().Format("2006-01-02 15:04"))
			if m.AsksMe {
				b.WriteString(", просит ответа от вас")
			}
		} else {
			fmt.Fprintf(&b, "Пришло сообщение от %s (id %s), %s", m.From, m.ID, m.At.Local().Format("2006-01-02 15:04"))
		}
		b.WriteString(":\n")
		b.WriteString(capRunes(m.Body, hookMaxBody))
		b.WriteString("\n")
		if m.ChatID != "" {
			fmt.Fprintf(&b, "Ответить: agentlink send --chat %s --body \"<текст>\"\n", m.ChatID)
		} else {
			fmt.Fprintf(&b, "Ответить: agentlink send --to %s --reply-to %s --body \"<текст>\"\n", m.From, m.ID)
		}
	}
	if rest := len(msgs) - len(shown); rest > 0 {
		fmt.Fprintf(&b, "\nИ ещё %d более ранних; вся переписка: agentlink chat list, agentlink chat history --chat <id>, agentlink inbox.\n", rest)
	}
	return strings.TrimRight(b.String(), "\n")
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + fmt.Sprintf("\n[... обрезано, всего %d символов; полностью: agentlink chat history / agentlink inbox]", len(r))
}

// hookGet calls the local API with a short timeout: a hook must not stall the
// session when the node is down.
func hookGet(api, path string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), hookHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL(config.Config{API: api}, path, q), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// errNotFound: the API has no such endpoint (a node from before chats).
var errNotFound = errors.New("not found")

var sessionJunk = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sessionFileName makes a session id safe as a file name.
func sessionFileName(id string) string {
	if len(id) <= 64 && !sessionJunk.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

// loadCursor reads a session's cursor; first is true when there is none yet.
func loadCursor(path string) (hookCursor, bool, error) {
	var cur hookCursor
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return hookCursor{Chats: map[string]uint64{}}, true, nil
	}
	if err != nil {
		return cur, false, err
	}
	if err := json.Unmarshal(data, &cur); err != nil {
		return hookCursor{Chats: map[string]uint64{}}, true, nil //nolint:nilerr // a broken cursor starts over
	}
	if cur.Chats == nil {
		cur.Chats = map[string]uint64{}
	}
	return cur, false, nil
}

// lockFile serializes hooks of one session (Claude Code runs PostToolUse hooks
// of parallel tool calls at the same time). A lock older than 30s is stale.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pruneCursors removes cursor files of sessions not seen for hookCursorTTL.
func pruneCursors(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && time.Since(info.ModTime()) > hookCursorTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// writeFileAtomic replaces path with data through a temporary file.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// mustJSON encodes v without HTML escaping; v is always encodable here.
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}
