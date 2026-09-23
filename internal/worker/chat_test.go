package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// fakeChatAgent is the "chat" fake agent: Claude stream-json with sessions
// kept as files in $AGENTLINK_FAKE_SESSIONS. "--session-id X" creates X,
// "--resume X" needs it (else it fails like Claude: no conversation found).
// It logs "start:X:CHAT" or "resume:X:CHAT" (CHAT from $AGENTLINK_CHAT_ID),
// edits a file, and answers with its stdin; stdin "hang" hangs after the
// session announcement, to be killed mid-run.
func fakeChatAgent() {
	in, _ := io.ReadAll(os.Stdin)
	dir := os.Getenv("AGENTLINK_FAKE_SESSIONS")
	mode, session := "", ""
	for i, a := range os.Args[:len(os.Args)-1] {
		switch a {
		case "--session-id":
			mode, session = "start", os.Args[i+1]
			_ = os.WriteFile(filepath.Join(dir, session), nil, 0o600)
		case "--resume":
			mode, session = "resume", os.Args[i+1]
		}
	}
	fakeLog(mode + ":" + session + ":" + os.Getenv("AGENTLINK_CHAT_ID"))
	if _, err := os.Stat(filepath.Join(dir, session)); err != nil || session == "" {
		_, _ = os.Stderr.WriteString("No conversation found with session ID: " + session)
		os.Exit(1)
	}
	line := func(v any) {
		data, _ := json.Marshal(v)
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
	line(map[string]any{"type": "system", "subtype": "init", "session_id": session})
	if strings.Contains(string(in), "hang") && mode == "resume" && !strings.Contains(string(in), ResumePrompt) {
		time.Sleep(time.Minute)
	}
	line(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"id": "m1", "content": []any{
		map[string]any{"type": "tool_use", "id": "t1", "name": "Edit", "input": map[string]any{"file_path": "notes.md"}},
	}}})
	time.Sleep(50 * time.Millisecond)
	line(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
	}}})
	line(map[string]any{"type": "result", "is_error": false, "session_id": session, "result": string(in)})
}

func chatAgent(t *testing.T) (Command, func() []string) {
	t.Helper()
	runs := fakeRunLog(t)
	t.Setenv("AGENTLINK_FAKE_SESSIONS", t.TempDir())
	agent := fakeAgent(t, "chat")
	agent.Format, agent.Preamble = FormatClaude, chatPreamble
	agent.SessionArgs = []string{"--session-id", SessionIDArg}
	agent.ResumeArgs = []string{"--resume", SessionIDArg}
	return agent, runs
}

const chatPreamble = "PREAMBLE\n"

const chatID = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"

// fakeChats is a node's chat side with one chat of me, peer and carol.
type fakeChats struct {
	mu     sync.Mutex
	chat   node.Chat
	msgs   []node.ChatMessage
	deny   bool
	claims int
}

func newFakeChats() *fakeChats {
	return &fakeChats{chat: node.Chat{ID: chatID, Participants: []string{"carol", "me", "peer"}}}
}

func (f *fakeChats) ClaimRun(m node.Message) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	return !f.deny && m.Asks("me") && !f.chat.Closed(), nil
}

func (f *fakeChats) ChatOf(id string) (node.Chat, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chat, id == f.chat.ID
}

func (f *fakeChats) ChatMessages(id string, _, after uint64, limit int) ([]node.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []node.ChatMessage
	for _, m := range f.msgs {
		if m.Seq > after {
			out = append(out, m)
		}
	}
	if after > 0 {
		return out[:min(limit, len(out))], nil
	}
	return out[max(0, len(out)-limit):], nil
}

// post stores m as the next chat message and returns it.
func (f *fakeChats) post(m node.Message) node.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.ChatID, m.Participants = chatID, f.chat.Participants
	f.msgs = append(f.msgs, node.ChatMessage{Seq: uint64(len(f.msgs) + 1), Message: m})
	return m
}

func (f *fakeChats) seqOf(id string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if m.ID == id {
			return m.Seq
		}
	}
	return 0
}

// chatRecorder also stores what the worker sends to the chat, as a node would.
func chatRecorder(chats *fakeChats) (*recorder, SendFunc) {
	rec := newRecorder()
	return rec, func(m node.Message) (node.Message, error) {
		if m.ChatID != "" && m.Kind == "" {
			m.From = "me"
			chats.post(m)
		}
		return rec.send(m)
	}
}

func chatWorker(t *testing.T, chats *fakeChats, send SendFunc, state, dir string, opt Options) *Worker {
	t.Helper()
	opt.Chats, opt.Self = chats, "me"
	w, err := New(nil, send, state, dir, opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func ask(chats *fakeChats, id, from, body string) node.Message {
	return chats.post(node.Message{ID: id, From: from, Body: body, Responders: []string{"me"}, RootID: id})
}

// Two requests of one chat run as two turns of one agent session: the second
// resumes the first's session and gets the chat messages since the first
// (a group message nobody was asked about included), not the old ones. Replies
// and statuses go to the chat under ids that include this node; activity
// carries its structured state.
func TestChatTurnsShareOneSession(t *testing.T) {
	agent, runs := chatAgent(t)
	chats := newFakeChats()
	rec, send := chatRecorder(chats)
	state := t.TempDir()
	w := chatWorker(t, chats, send, state, t.TempDir(), Options{Agent: func() Command { return agent }, ActivityEvery: 10 * time.Millisecond})
	start(t, w)

	m1 := ask(chats, id1, "peer", "first question")
	accept(t, w, m1)
	r1 := rec.wait(t, 1)[0]
	if r1.JobStatus != node.JobCompleted || r1.ChatID != chatID || r1.To != "" || r1.ID != node.DerivedID(id1, "me/reply") ||
		!strings.Contains(r1.Body, "first question") || !strings.HasPrefix(r1.Body, chatPreamble) {
		t.Fatalf("first reply = %+v", r1)
	}
	chats.post(node.Message{ID: id2, From: "carol", Body: "fyi from carol"})
	m3 := ask(chats, id3, "peer", "second question")
	accept(t, w, m3)
	r2 := rec.wait(t, 1)[1]
	if r2.JobStatus != node.JobCompleted || strings.HasPrefix(r2.Body, chatPreamble) ||
		!strings.Contains(r2.Body, "second question") || !strings.Contains(r2.Body, "carol: fyi from carol") ||
		strings.Contains(r2.Body, "first question") {
		t.Fatalf("second reply = %+v", r2)
	}
	r := runs()
	if len(r) != 2 || !strings.HasPrefix(r[0], "start:") || !strings.HasPrefix(r[1], "resume:") {
		t.Fatalf("agent runs %v", r)
	}
	session := strings.Split(r[0], ":")[1]
	if r[0] != "start:"+session+":"+chatID || r[1] != "resume:"+session+":"+chatID {
		t.Fatalf("agent runs %v, want one session in chat %s", r, chatID)
	}
	data, err := os.ReadFile(filepath.Clean(filepath.Join(state, "sessions", chatID+".json")))
	if err != nil {
		t.Fatal(err)
	}
	var s AgentSession
	if err := json.Unmarshal(data, &s); err != nil || s.SessionID != session || s.LastSeq != chats.seqOf(id3) || s.Provider != FormatClaude {
		t.Fatalf("session %+v, %v", s, err)
	}
	for _, m := range rec.filter(func(m node.Message) bool { return m.Kind == node.KindStatus }) {
		if m.ChatID != chatID || m.To != "" {
			t.Fatalf("status %+v not sent to the chat", m)
		}
	}
	acts := rec.filter(func(m node.Message) bool { return m.ActivityInfo != nil })
	if len(acts) == 0 {
		t.Fatal("no structured activity")
	}
	for _, m := range acts {
		a := m.ActivityInfo
		if a.ID != "t1" || a.Type != TypeEdit || a.Text != "Edit notes.md" || m.Activity != a.Text || a.StartedAt.IsZero() || a.Seq == 0 {
			t.Fatalf("activity %+v", a)
		}
	}
}

// A chat runs one job at a time; other chats and direct requests use the
// other slots meanwhile.
func TestChatRunsOneJobAtATime(t *testing.T) {
	chats := newFakeChats()
	rec, send := chatRecorder(chats)
	release := make(chan struct{})
	var mu sync.Mutex
	running := map[string]bool{}
	run := func(ctx context.Context, _, prompt string, _ func(string)) (string, error) {
		key := strings.Fields(prompt)[0]
		mu.Lock()
		running[key] = true
		mu.Unlock()
		<-release
		mu.Lock()
		delete(running, key)
		mu.Unlock()
		return "done " + key, nil
	}
	w, err := New(run, send, t.TempDir(), t.TempDir(), Options{MaxJobs: 3, Chats: chats, Self: "me"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start(t, w)
	accept(t, w, ask(chats, id1, "peer", "a"))
	accept(t, w, ask(chats, id2, "carol", "b"))
	accept(t, w, msg(id3, "direct"))
	busy := func(keys ...string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, k := range keys {
			if !running[k] {
				return false
			}
		}
		return len(running) == len(keys)
	}
	eventually(t, "one chat job and the direct one", func() bool { return busy("a", "direct") })
	time.Sleep(100 * time.Millisecond)
	if !busy("a", "direct") {
		t.Fatal("a second job of the chat started")
	}
	if j, _ := w.Job(id2); j.Status != node.JobQueued {
		t.Fatalf("second chat job = %s, want queued", j.Status)
	}
	close(release)
	rec.wait(t, 3)
}

// A chat closed while its request waited fails the request without running
// it; the node's refusal (ClaimRun) creates no job at all.
func TestChatClosedAndUnclaimed(t *testing.T) {
	chats := newFakeChats()
	rec, send := chatRecorder(chats)
	var runs atomic.Int32
	run := func(context.Context, string, string, func(string)) (string, error) { runs.Add(1); return "x", nil }
	w, err := New(run, send, t.TempDir(), t.TempDir(), Options{Chats: chats, Self: "me"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	accept(t, w, ask(chats, id1, "peer", "q"))
	chats.mu.Lock()
	chats.chat.CloseID, chats.chat.ClosedBy = id3, "peer"
	chats.deny = true
	chats.mu.Unlock()
	accept(t, w, ask(chats, id2, "peer", "late"))
	start(t, w)
	got := rec.wait(t, 1)[0]
	if got.ReplyTo != id1 || got.JobStatus != node.JobFailed || got.Body != "agentlink: "+ErrChatClosed || got.ChatID != chatID {
		t.Fatalf("reply = %+v", got)
	}
	rec.quiet(t)
	if _, ok := w.Job(id2); ok || runs.Load() != 0 {
		t.Fatalf("unclaimed request queued or a job ran (runs %d)", runs.Load())
	}
	// Status updates never reach ClaimRun.
	chats.mu.Lock()
	before := chats.claims
	chats.mu.Unlock()
	accept(t, w, chats.post(node.Message{ID: "00000000000000000000000000000009", From: "peer", Body: "fyi", Kind: node.KindStatus, ReplyTo: id1}))
	chats.mu.Lock()
	defer chats.mu.Unlock()
	if chats.claims != before {
		t.Fatal("a status update was claimed")
	}
}

// A chat whose session the agent lost fails with the advice to start a new
// chat; a session of another agent or project fails before any run.
func TestChatSessionProblems(t *testing.T) {
	for _, c := range []struct {
		name, provider, want string
		runs                 int
	}{
		{"missing", FormatClaude, ErrSessionMissing, 1},
		{"other agent", FormatCodex, ErrSessionMismatch, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			agent, runs := chatAgent(t)
			chats := newFakeChats()
			rec, send := chatRecorder(chats)
			state, dir := t.TempDir(), t.TempDir()
			w := chatWorker(t, chats, send, state, dir, Options{Agent: func() Command { return agent }})
			if err := writeAtomic(filepath.Join(state, "sessions"), chatID+".json",
				AgentSession{ChatID: chatID, Provider: c.provider, Dir: filepath.Clean(dir), SessionID: "gone"}); err != nil {
				t.Fatal(err)
			}
			start(t, w)
			accept(t, w, ask(chats, id1, "peer", "q"))
			got := rec.wait(t, 1)[0]
			if got.JobStatus != node.JobFailed || !strings.HasPrefix(got.Body, "agentlink: "+c.want) {
				t.Fatalf("reply = %+v", got)
			}
			if c.runs == 1 && !strings.Contains(got.Body, "No conversation found") {
				t.Fatalf("reply without the agent's reason: %q", got.Body)
			}
			if r := runs(); len(r) != c.runs {
				t.Fatalf("agent runs %v", r)
			}
		})
	}
}

// A new turn cut off by a restart (the agent died too) resumes the chat's
// session with ResumePrompt, without feeding the request again, and still
// advances the session past it.
func TestChatTurnRecovery(t *testing.T) {
	agent, runs := chatAgent(t)
	chats := newFakeChats()
	rec, send := chatRecorder(chats)
	state, dir := t.TempDir(), t.TempDir()
	opt := Options{Agent: func() Command { return agent }}
	w1 := chatWorker(t, chats, send, state, dir, opt)
	stop := start(t, w1)
	accept(t, w1, ask(chats, id1, "peer", "first"))
	rec.wait(t, 1)
	accept(t, w1, ask(chats, id2, "peer", "hang"))
	eventually(t, "second turn running", func() bool { return len(runs()) == 2 })
	p := agentPID(t, w1, id2)
	stop()
	if err := killTree(t.Context(), p.PID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent dead", func() bool { return !alive(p) })
	w2 := chatWorker(t, chats, send, state, dir, opt)
	start(t, w2)
	got := rec.wait(t, 1)[1]
	if got.ReplyTo != id2 || got.JobStatus != node.JobCompleted || got.Body != strings.TrimSpace(ResumePrompt) {
		t.Fatalf("reply = %+v", got)
	}
	r := runs()
	session := strings.Split(r[0], ":")[1]
	if want := []string{"start:" + session + ":" + chatID, "resume:" + session + ":" + chatID, "resume:" + session + ":" + chatID}; !slices.Equal(r, want) {
		t.Fatalf("agent runs %v, want %v", r, want)
	}
	j, _ := w2.Job(id2)
	if j.Attempts != 2 || j.Proc == nil || !j.Proc.Resumed || !j.Proc.Continued || j.InputThrough != chats.seqOf(id2) {
		t.Fatalf("job = %+v", j)
	}
	w2.mu.Lock()
	s, err := w2.session(chatID)
	w2.mu.Unlock()
	if err != nil || s.LastSeq != chats.seqOf(id2) {
		t.Fatalf("session %+v, %v", s, err)
	}
}
