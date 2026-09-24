package main

// agentlink hook: connects a live interactive Claude Code or Codex session to
// this node through the agent's own hooks. Nobody can type into a session on
// another machine, but its hooks run this command:
//
//   - SessionStart registers the session (POST /sessions) for its folder, every
//     event is a heartbeat, SessionEnd ends it (DELETE /sessions/{id}).
//   - SessionStart, UserPromptSubmit, PostToolUse and Stop hand the model the
//     node's unread messages for the session's folder (GET /unread) and then
//     acknowledge them (POST /ack), so each is delivered once. Stop blocks
//     only when there is something new.
//   - The person at the session sees a short line (systemMessage).
//   - While the session works on a batch it accepted, PreToolUse, UserPromptSubmit
//     and Stop report what it does (POST /chats/{id}/activity).
//   - Claude Code also runs `agentlink hook claude --wait` in the background
//     (asyncRewake): it waits for unread messages and wakes an idle session;
//     see hook_wait.go. Codex has no such hook: it hears of messages at its
//     next event.
//
// A hook never breaks or stalls its session: every failure (node down, bad
// input) ends quietly with exit 0.

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
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	evPreTool      = agenthook.PreTool
	evPostTool     = agenthook.PostTool
	evStop         = agenthook.Stop
	evSessionEnd   = agenthook.SessionEnd
)

// Limits and timings of the hook.
const (
	hookBudget      = 4500 // runes of message text in one batch (Codex: ~2500 tokens per hook output)
	hookMaxBody     = 2500 // runes of one message body
	hookPageSize    = 50   // unread messages fetched per event
	hookHTTPTimeout = 1500 * time.Millisecond
	hookStateTTL    = 14 * 24 * time.Hour // state files older than this are removed
	// hookHeartbeat: a session re-registers at an event at most this often.
	hookHeartbeat = time.Minute
	// hookMaxStopBlocks: consecutive Stop continuations (stop_hook_active)
	// before Stop lets the session end and leaves the rest for later.
	hookMaxStopBlocks = 3
	// Session TTLs asked of the node. Claude's background waiter heartbeats
	// while the session is idle; Codex has only its events.
	hookTTLClaude = 900
	hookTTLCodex  = 3600
)

// hookInput is the part of the hook's stdin JSON this command reads. Claude
// Code and Codex send the same fields.
type hookInput struct {
	SessionID      string          `json:"session_id"`
	HookEventName  string          `json:"hook_event_name"`
	StopHookActive bool            `json:"stop_hook_active"`
	Cwd            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// hookState is what the hooks of one session keep between events.
type hookState struct {
	Folder string `json:"folder,omitempty"`
	// Registered: the last heartbeat the node answered; Unbound: it refused
	// the folder (not the working folder or a project folder).
	Registered time.Time `json:"registered,omitzero"`
	Unbound    bool      `json:"unbound,omitempty"`
	Ended      bool      `json:"ended,omitempty"` // SessionEnd ran: the waiter stops
	// LastEvent is the latest hook event (the waiter wakes only an idle session).
	LastEvent   string    `json:"last_event,omitempty"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`
	// Active: chats whose requests this session accepted and works on, with
	// the request activity is reported for.
	Active map[string]string `json:"active,omitempty"`
	// Activity is the last activity posted, for coalescing.
	Activity   string    `json:"activity,omitempty"`
	ActivityAt time.Time `json:"activity_at,omitzero"`
	Blocks     int       `json:"blocks,omitempty"` // consecutive Stop blocks
	// Notice: a line for the person, shown at the next event (the waiter
	// cannot show one itself).
	Notice string `json:"notice,omitempty"`
	// Notes for the model at its next event (a request the worker took after
	// the session was told about it).
	Notes []string `json:"notes,omitempty"`
}

// hookEnv is where the hook finds the node and keeps its state.
type hookEnv struct {
	api string // host:port of the node's local API
	dir string // state directory
	now func() time.Time
}

func (e hookEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// runHook runs `agentlink hook ...`. A hook never breaks the session it runs
// in: every failure (no node, bad input) ends with exit 0 and no output.
func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "install" {
		return runHookInstall(args[1:], stdout, stderr)
	}
	if len(args) == 0 || (args[0] != hookClaude && args[0] != hookCodex) {
		_, _ = fmt.Fprintln(stderr, "usage: agentlink hook <claude|codex> [--event auto] [--wait] | agentlink hook install <claude|codex> [--scope user|project]")
		return 1
	}
	client := args[0]
	flags := flag.NewFlagSet("hook", flag.ContinueOnError)
	flags.SetOutput(stderr)
	event := flags.String("event", "auto", "hook event; auto: hook_event_name of the input")
	wait := flags.Bool("wait", false, "wait in the background for unread messages and wake the session (Claude Code asyncRewake)")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	// An agent the worker started for a job already gets its request.
	if os.Getenv(envJobID) != "" {
		return 0
	}
	env, err := defaultHookEnv()
	if err != nil {
		return 0
	}
	if *wait {
		return hookWait(client, stdin, stderr, env, defaultWaitOpts())
	}
	// The output is written before the batch is acknowledged (see accept).
	_ = hookRun(client, *event, stdin, stdout, env)
	return 0
}

// defaultHookEnv finds the node API like the other client commands and keeps
// state in %APPDATA%\agentlink\hooks.
func defaultHookEnv() (hookEnv, error) {
	cfg, err := loadConfig("hook", "")
	if err != nil {
		return hookEnv{}, err
	}
	p, err := settings.DefaultPath()
	if err != nil {
		return hookEnv{}, err
	}
	return hookEnv{api: cfg.API, dir: filepath.Join(filepath.Dir(p), "hooks")}, nil
}

// hookRun handles one hook event: it reads the input, keeps the session
// registered, delivers unread messages, reports activity and writes the
// event's JSON output to stdout (nothing when there is nothing to say; Codex's
// Stop always gets a JSON object).
func hookRun(client, event string, stdin io.Reader, stdout io.Writer, env hookEnv) error {
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return err
	}
	if event == "" || event == "auto" {
		event = in.HookEventName
	}
	if !slices.Contains(agenthook.Events, event) || in.SessionID == "" {
		return nil
	}
	quiet := func() {
		if client == hookCodex && event == evStop {
			_, _ = io.WriteString(stdout, "{}\n") // Codex wants JSON from a Stop hook that exits 0
		}
	}
	folder := hookFolder(in.Cwd)
	path := hookStatePath(env.dir, client, in.SessionID)
	if event == evSessionEnd {
		endSession(env, path, in.SessionID)
		return nil
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		quiet()
		return err
	}
	defer unlock()
	if event == evSessionStart {
		pruneHookState(env.dir, env.clock())
	}
	st := loadHookState(path)
	now := env.clock()
	st.LastEvent, st.LastEventAt = event, now
	defer func() { _ = saveHookState(path, st) }()
	if !heartbeat(env, &st, client, in.SessionID, folder, event == evSessionStart) {
		quiet()
		return nil
	}
	h := &hookSession{env: env, st: &st, sid: in.SessionID, folder: folder}
	if event == evPreTool {
		typ, text := toolActivity(folder, in.ToolName, in.ToolInput)
		h.report(typ, text, "")
		writeHookJSON(stdout, takeNotice(&st), nil)
		return nil
	}
	b, err := h.collect(event == evStop)
	if err != nil {
		quiet()
		return err
	}
	notes := st.Notes
	if event == evStop {
		switch {
		case b.empty():
			st.Blocks = 0
			h.idle()
			if notice := takeNotice(&st); notice != "" {
				writeHookJSON(stdout, notice, nil)
			} else {
				quiet()
			}
			return nil
		case in.StopHookActive && st.Blocks >= hookMaxStopBlocks:
			h.idle() // the turn ends all the same: its activity too
			quiet()  // let it end; the waiter or the next event delivers the rest
			return nil
		}
		st.Blocks++
		st.Notes = nil
		writeHookJSON(stdout, joinNotice(takeNotice(&st), b.notice), map[string]any{"decision": "block", "reason": withNotes(notes, b.text)})
		h.accept(b)
		return nil
	}
	if event == evPrompt {
		h.report("thinking", "думает", "")
	}
	text := withNotes(notes, b.text)
	notice := joinNotice(takeNotice(&st), b.notice)
	if text == "" && notice == "" {
		return nil
	}
	st.Notes = nil
	var extra map[string]any
	if text != "" {
		extra = map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": event, "additionalContext": text}}
	}
	writeHookJSON(stdout, notice, extra)
	h.accept(b)
	return nil
}

// hookFolder is the session's folder: its cwd, absolute and clean.
func hookFolder(cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		return abs
	}
	return filepath.Clean(cwd)
}

// heartbeat registers the session (again) when due: at SessionStart, when its
// folder changed, or hookHeartbeat after the last time. It reports whether the
// session is registered for a folder of this node; false also when the node is
// down.
func heartbeat(env hookEnv, st *hookState, client, sid, folder string, force bool) bool {
	now := env.clock()
	if !force && st.Folder == folder && now.Sub(st.Registered) < hookHeartbeat {
		return !st.Unbound
	}
	wake, ttl := node.WakeNextEvent, hookTTLCodex
	if client == hookClaude {
		wake, ttl = node.WakeRewake, hookTTLClaude
	}
	req := node.SessionRequest{SessionID: sid, Provider: client, Folder: folder, Wake: wake, TTLSec: ttl}
	err := hookCall(env.api, http.MethodPost, "/sessions", nil, req, nil, hookHTTPTimeout)
	var se *statusError
	switch {
	case err == nil:
		st.Folder, st.Registered, st.Unbound, st.Ended = folder, now, false, false
		return true
	case errors.As(err, &se) && se.code == http.StatusBadRequest:
		st.Folder, st.Registered, st.Unbound = folder, now, true // not a folder of this node
		return false
	default:
		return false // node down: try again at the next event
	}
}

// endSession ends the session's activity, deregisters it and marks its state
// ended, which stops its waiter.
func endSession(env hookEnv, path, sid string) {
	unlock, err := lockFile(path + ".lock")
	if err == nil {
		defer unlock()
	}
	st := loadHookState(path)
	(&hookSession{env: env, st: &st, sid: sid, folder: st.Folder}).idle()
	st.Ended, st.Active = true, nil
	_ = saveHookState(path, st)
	_ = hookCall(env.api, http.MethodDelete, "/sessions/"+url.PathEscape(sid), nil, nil, nil, hookHTTPTimeout)
}

// writeHookJSON writes the event's JSON output: extra's fields plus
// systemMessage, the line the person at the session sees. Nothing at all when
// both are empty.
func writeHookJSON(w io.Writer, notice string, extra map[string]any) {
	if notice == "" && len(extra) == 0 {
		return
	}
	v := map[string]any{}
	maps.Copy(v, extra)
	if notice != "" {
		v["systemMessage"] = notice
	}
	_, _ = w.Write(mustJSON(v))
}

func takeNotice(st *hookState) string {
	n := st.Notice
	st.Notice = ""
	return n
}

func joinNotice(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "\n" + b
}

func withNotes(notes []string, text string) string {
	if len(notes) == 0 {
		return text
	}
	return strings.TrimSpace(strings.Join(notes, "\n") + "\n\n" + text)
}

// hookSession is one hook run of a session, with its state held under lock.
type hookSession struct {
	env    hookEnv
	st     *hookState
	sid    string
	folder string
}

// hookBatch is the unread messages one event delivers.
type hookBatch struct {
	text   string   // for the model
	notice string   // for the person
	ids    []string // to acknowledge
	// chats: chat id -> the request of it this session is to answer.
	chats map[string]string
}

func (b hookBatch) empty() bool { return len(b.ids) == 0 }

// collect reads the unread messages of the session's folder and formats what
// fits one hook output. The rest stays unread for the next event.
func (h *hookSession) collect(stop bool) (hookBatch, error) {
	var page node.UnreadPage
	q := url.Values{"folder": {h.folder}, "limit": {fmt.Sprint(hookPageSize)}}
	if err := hookCall(h.env.api, http.MethodGet, "/unread", q, nil, &page, hookHTTPTimeout); err != nil {
		return hookBatch{}, err
	}
	return formatBatch(page, h.folder, h.sid, stop), nil
}

// accept acknowledges a delivered batch (after it was written out), marks its
// chats as the ones this session works on and tells them it started. A
// request the worker took meanwhile becomes a note for the next event.
func (h *hookSession) accept(b hookBatch) {
	if b.empty() {
		return
	}
	var res []node.AckResult
	req := node.AckRequest{IDs: b.ids, SessionID: h.sid}
	if err := hookCall(h.env.api, http.MethodPost, "/ack", nil, req, &res, hookHTTPTimeout); err != nil {
		return // still unread: delivered again at the next event
	}
	for _, r := range res {
		if r.Assigned != "worker" {
			continue
		}
		for chat, id := range b.chats {
			if id == r.ID {
				delete(b.chats, chat)
				h.st.Notes = append(h.st.Notes, fmt.Sprintf("agent-link: сообщение %s в чате %s уже взял агент-обработчик этого узла — не отвечайте на него.", r.ID, chat))
			}
		}
	}
	if len(b.chats) == 0 {
		return
	}
	if h.st.Active == nil {
		h.st.Active = map[string]string{}
	}
	maps.Copy(h.st.Active, b.chats)
	h.st.Activity = "" // a new batch always shows
	h.report("read", "читает сообщения", "")
}

// formatBatch turns an unread page into the text for the model and the line
// for the person, within hookBudget runes.
func formatBatch(page node.UnreadPage, folder, sid string, stop bool) hookBatch {
	b := hookBatch{chats: map[string]string{}}
	if len(page.Messages) == 0 {
		return b
	}
	var body strings.Builder
	used := 0
	var shown []node.UnreadMessage
	for _, m := range page.Messages {
		entry := formatUnread(m)
		n := utf8.RuneCountInString(entry)
		if len(shown) > 0 && used+n > hookBudget {
			break
		}
		used += n
		shown = append(shown, m)
		body.WriteString("\n")
		body.WriteString(entry)
		b.ids = append(b.ids, m.ID)
		if m.ChatID != "" && m.AsksYou && !m.Paused && m.Assigned != "worker" {
			b.chats[m.ChatID] = m.ID
		}
	}
	var t strings.Builder
	if stop {
		t.WriteString("Пока вы работали, пришли сообщения agent-link. Прочитайте их и, где просят ответа, ответьте; затем завершайте.\n")
	} else {
		fmt.Fprintf(&t, "agent-link: непрочитанные сообщения для этой папки (%d). Вся переписка остаётся в истории чата.\n", page.Total)
	}
	t.WriteString(body.String())
	if rest := page.Total - len(shown); rest > 0 {
		fmt.Fprintf(&t, "\nЕщё %d непрочитанных придут со следующим событием. Прочитать сейчас: agentlink chat unread --folder %q --after %s ; прочитанные подтвердить: agentlink chat ack --ids <id,...> --session %s\n",
			rest, folder, shown[len(shown)-1].Cursor, sid)
	}
	b.text = strings.TrimRight(t.String(), "\n")
	b.notice = batchNotice(shown)
	return b
}

// formatUnread is one message for the model: who wrote it, where, the text
// and what to do with it.
func formatUnread(m node.UnreadMessage) string {
	var b strings.Builder
	at := m.CreatedAt.Local().Format("2006-01-02 15:04")
	switch {
	case m.OwnHuman:
		fmt.Fprintf(&b, "Ваш человек написал всем (%s, чат %s, id %s) — к сведению, отвечать не нужно:\n", at, m.ChatID, m.ID)
	case m.ChatID != "":
		fmt.Fprintf(&b, "От %s (%s) в чате %s (участники: %s), id %s, %s:\n", m.From, authorKind(m.AuthorKind), m.ChatID, strings.Join(m.Participants, ", "), m.ID, at)
	default:
		fmt.Fprintf(&b, "От %s (%s), id %s, %s:\n", m.From, authorKind(m.AuthorKind), m.ID, at)
	}
	b.WriteString(capBody(m))
	b.WriteString("\n")
	if m.OwnHuman {
		return b.String()
	}
	reply := fmt.Sprintf("agentlink send --to %s --reply-to %s --body \"<текст>\"", m.From, m.ID)
	if m.ChatID != "" {
		reply = fmt.Sprintf("agentlink send --chat %s --reply-to %s --body \"<текст>\"", m.ChatID, m.ID)
	}
	switch {
	case m.Assigned == "worker":
		b.WriteString("Это уже обрабатывает агент-обработчик этого узла — не отвечайте.\n")
	case m.Paused:
		b.WriteString("Пауза: агенты ответили друг другу слишком много раз подряд, нужен человек — не отвечайте автоматически.\n")
	case m.AsksYou:
		fmt.Fprintf(&b, "Просит ответа от вас. Ответить: %s\n", reply)
	default:
		fmt.Fprintf(&b, "К сведению, ответ не обязателен. Ответить: %s\n", reply)
	}
	return b.String()
}

func authorKind(k string) string {
	switch k {
	case "human":
		return "человек"
	case "agent":
		return "агент"
	case "worker":
		return "агент-обработчик"
	}
	return "автор не указан"
}

// capBody cuts a long body, pointing at the command that shows all of it.
func capBody(m node.UnreadMessage) string {
	if utf8.RuneCountInString(m.Body) <= hookMaxBody {
		return m.Body
	}
	full := "agentlink inbox"
	if m.ChatID != "" {
		full = "agentlink chat history --chat " + m.ChatID
	}
	r := []rune(m.Body)
	return string(r[:hookMaxBody]) + fmt.Sprintf("\n[… обрезано, всего %d символов; полностью: %s]", len(r), full)
}

// batchNotice is the line the person sees: «agent-link: 2 сообщения от
// KPECTIK — беру в работу». The person's own messages need no line.
func batchNotice(msgs []node.UnreadMessage) string {
	var from []string
	n, mine, worker := 0, false, true
	for _, m := range msgs {
		if m.OwnHuman {
			continue
		}
		n++
		if !slices.Contains(from, m.From) {
			from = append(from, m.From)
		}
		if m.Assigned != "worker" {
			worker = false
			if m.AsksYou && !m.Paused {
				mine = true
			}
		}
	}
	if n == 0 {
		return ""
	}
	what := "к сведению"
	switch {
	case mine:
		what = "беру в работу"
	case worker:
		what = "отвечает агент-обработчик"
	}
	return fmt.Sprintf("agent-link: %d %s от %s — %s", n, plural(n, "сообщение", "сообщения", "сообщений"), strings.Join(from, ", "), what)
}

// plural picks the Russian form for n: 1 сообщение, 2 сообщения, 5 сообщений.
func plural(n int, one, few, many string) string {
	switch n10, n100 := n%10, n%100; {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	}
	return many
}

// statusError is a non-2xx answer of the node.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("node: %d %s", e.code, e.msg) }

// hookCall calls the local API with a short timeout: a hook must not stall
// the session when the node is down. body (when not nil) is sent as JSON, a
// JSON answer is decoded into out (when not nil).
func hookCall(api, method, path string, q url.Values, body, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL(config.Config{API: api}, path, q), rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{code: resp.StatusCode, msg: strings.TrimSpace(string(msg))}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var sessionJunk = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sessionFileName makes a session id safe as a file name.
func sessionFileName(id string) string {
	if len(id) <= 64 && !sessionJunk.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func hookStatePath(dir, client, sid string) string {
	return filepath.Join(dir, client+"-"+sessionFileName(sid)+".json")
}

// loadHookState reads a session's state; a missing or broken file is a new
// session.
func loadHookState(path string) hookState {
	var st hookState
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil && json.Unmarshal(data, &st) != nil {
		st = hookState{}
	}
	return st
}

func saveHookState(path string, st hookState) error {
	return writeFileAtomic(path, mustJSON(st))
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
		time.Sleep(20 * time.Millisecond)
	}
}

// pruneHookState removes state files of sessions not seen for hookStateTTL.
func pruneHookState(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && now.Sub(info.ModTime()) > hookStateTTL {
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
