package worker

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A chat request (Message.ChatID) runs in the chat's own agent session, kept
// in <state dir>/sessions/<chat id>.json across requests and restarts: the
// first request of a chat starts a new session, every later one resumes it
// (Command.ResumeArgs) with the new request and the chat messages this node's
// agent has not seen yet (LastSeq), so the agent keeps the conversation. At
// most one job of a chat runs at a time. A session belongs to the agent
// (provider) and the directory it started in: when either changed, or the
// agent lost the session, the request fails with the advice to start a new
// chat. An agent without ResumeArgs gets a fresh run with the latest chat
// messages each time.

// Chats is the chat side of the node the worker answers for; node.Node
// implements it.
type Chats interface {
	// ClaimRun reports whether this node should answer chat message m (see
	// node.Node.ClaimRun); it is asked once per new request. When m must not
	// run, hold is the reason (node.Hold*), "" when m does not ask this node.
	ClaimRun(m node.Message) (run bool, hold string, err error)
	ChatOf(id string) (node.Chat, bool)
	ChatMessages(id string, before, after uint64, limit int) ([]node.ChatMessage, error)
}

// AgentSession is the local agent session of one chat. It never leaves this
// machine.
type AgentSession struct {
	ChatID    string `json:"chat_id"`
	Provider  string `json:"provider"` // the agent's output format, or its program name
	Dir       string `json:"dir"`
	SessionID string `json:"session_id"`
	// LastSeq is the last chat message (node's Seq) the session was given.
	LastSeq uint64 `json:"last_seq"`
}

// Failure texts of chat jobs.
const (
	ErrChatClosed      = "the chat was closed before this request started"
	ErrSessionMismatch = "this chat's agent session was started by another agent or in another project; start a new chat"
	ErrSessionMissing  = "this chat's agent session could not be resumed; start a new chat"
)

// Limits of the chat messages fed into a run.
const (
	chatFetch      = 1000 // messages read since the session's LastSeq
	chatContext    = 50   // messages shown to the agent, the latest ones
	chatBodyRunes  = 2000 // one message's text is cut after this
	envAPI         = "AGENTLINK_API"
	envChatID      = "AGENTLINK_CHAT_ID"
	envJobID       = "AGENTLINK_JOB_ID"
	chatHistoryCmd = "agentlink chat history"
)

// agentEnv is the environment added to a job's agent: the node's API (so the
// agentlink CLI needs no --config) and, for a chat job, the chat and request.
func (w *Worker) agentEnv(chatID, jobID string) []string {
	var env []string
	if w.opt.API != "" {
		env = append(env, envAPI+"="+w.opt.API)
	}
	if chatID != "" {
		env = append(env, envChatID+"="+chatID, envJobID+"="+jobID)
	}
	return env
}

func provider(format, name string) string { return cmp.Or(format, filepath.Base(name)) }

func (w *Worker) sessionPath(chatID string) string {
	return filepath.Join(w.sessionsDir, chatID+".json")
}

// session reads a chat's agent session; a chat without one gives a zero
// session and no error. The caller holds w.mu.
func (w *Worker) session(chatID string) (AgentSession, error) {
	data, err := os.ReadFile(w.sessionPath(chatID))
	if errors.Is(err, fs.ErrNotExist) {
		return AgentSession{ChatID: chatID}, nil
	}
	if err != nil {
		return AgentSession{}, err
	}
	var s AgentSession
	if err := json.Unmarshal(data, &s); err != nil {
		return AgentSession{}, fmt.Errorf("session of chat %s: %w", chatID, err)
	}
	return s, nil
}

// noteSession records the session id a run announced, on its job and as the
// chat's session, at once: a later request must find it even when this run
// fails or the app stops.
func (w *Worker) noteSession(j *Job, id string) {
	if id == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if j.Proc != nil && j.Proc.Session != id {
		j.Proc.Session = id
		if err := w.save(j); err != nil {
			w.log.Error("save job", "id", j.Request.ID, "err", err)
		}
	}
	chatID := j.Request.ChatID
	if chatID == "" || j.Proc == nil {
		return
	}
	s, err := w.session(chatID)
	if err != nil {
		w.log.Error("read session", "chat", chatID, "err", err)
		return
	}
	p, dir := provider(j.Proc.Format, j.Proc.Name), j.Proc.Dir
	if s.SessionID == id && s.Provider == p && s.Dir == dir {
		return
	}
	s.SessionID, s.Provider, s.Dir = id, p, dir
	if err := writeAtomic(w.sessionsDir, chatID+".json", s); err != nil {
		w.log.Error("save session", "chat", chatID, "err", err)
	}
}

// advanceSession marks the chat messages a completed job was given as seen.
func (w *Worker) advanceSession(j *Job) {
	w.mu.Lock()
	defer w.mu.Unlock()
	chatID, through := j.Request.ChatID, j.InputThrough
	if chatID == "" || through == 0 {
		return
	}
	s, err := w.session(chatID)
	if err != nil || s.SessionID == "" || s.LastSeq >= through {
		return
	}
	s.LastSeq = through
	if err := writeAtomic(w.sessionsDir, chatID+".json", s); err != nil {
		w.log.Error("save session", "chat", chatID, "err", err)
	}
}

// chatLaunch prepares the first run of a chat job: a new turn of the chat's
// session when it has one and c can resume, else a new session. It records
// on the job which chat messages the run is given (InputThrough).
func (w *Worker) chatLaunch(j *Job, c Command, dir string) (launchSpec, error) {
	w.mu.Lock()
	m := j.Request
	s, err := w.session(m.ChatID)
	w.mu.Unlock()
	if err != nil {
		return launchSpec{}, err
	}
	if s.SessionID != "" && (s.Provider != provider(c.Format, c.Name) || s.Dir != filepath.Clean(dir)) {
		return launchSpec{}, errors.New(ErrSessionMismatch)
	}
	resume := s.SessionID != "" && len(c.ResumeArgs) > 0
	after := uint64(0)
	if resume {
		after = s.LastSeq
	}
	text, through := w.chatInput(m, after, resume)
	w.mu.Lock()
	j.InputThrough = through
	err = w.save(j)
	w.mu.Unlock()
	if err != nil {
		return launchSpec{}, err
	}
	if resume {
		return launchSpec{stdin: text, session: s.SessionID, continued: true}, nil
	}
	return launchSpec{stdin: c.Preamble + text}, nil
}

// chatInput is what the agent reads for chat request m: the request, then the
// chat's other messages after Seq after and before m (the latest chatContext
// of them; this node's own are in the session already), and how to read or
// write more. through is m's Seq, or after when m is not found.
func (w *Worker) chatInput(m node.Message, after uint64, resumed bool) (text string, through uint64) {
	through = after
	var seen []node.ChatMessage
	msgs, err := w.opt.Chats.ChatMessages(m.ChatID, 0, after, chatFetch)
	if err != nil {
		w.log.Warn("chat messages", "chat", m.ChatID, "err", err)
	}
	for _, cm := range msgs {
		if cm.ID == m.ID {
			through = cm.Seq
			break
		}
		if cm.Kind == "" && cm.From != w.opt.Self {
			seen = append(seen, cm)
		}
	}
	omitted := max(0, len(seen)-chatContext)
	seen = seen[omitted:]
	var b strings.Builder
	if resumed {
		b.WriteString("A new request in the same agent-link chat.\n")
	}
	b.WriteString(m.Body)
	b.WriteString("\n\n---\nagent-link chat " + m.ChatID)
	if parts := m.Participants; len(parts) > 0 {
		b.WriteString(", members: " + strings.Join(parts, ", "))
	}
	b.WriteString("; you are " + w.opt.Self + ". The request above is from " + m.From + "; your final answer goes to the whole chat.\n")
	if len(seen) > 0 {
		if resumed {
			b.WriteString("Chat messages since your last turn:\n")
		} else {
			b.WriteString("Earlier chat messages:\n")
		}
		if omitted > 0 {
			b.WriteString("(" + strconv.Itoa(omitted) + " older messages not shown)\n")
		}
		for _, cm := range seen {
			b.WriteString("- " + cm.From)
			if len(cm.Responders) > 0 {
				b.WriteString(" (asks " + strings.Join(cm.Responders, ", ") + ")")
			}
			b.WriteString(": " + cutRunes(strings.TrimSpace(cm.Body), chatBodyRunes) + "\n")
		}
	}
	set := "$" + envChatID + " is set"
	if w.opt.API != "" {
		set = "$" + envChatID + " and $" + envAPI + " are set, no --config needed"
	}
	b.WriteString("Read more of this chat with `" + chatHistoryCmd + "` (" + set + "); ask a member with `agentlink send --ask NAME --body TEXT`. Do not close the chat: only people do.\n")
	return b.String(), through
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// writeAtomic writes v as JSON to dir/name atomically (temp file, fsync, rename).
func writeAtomic(dir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
