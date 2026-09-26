package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seatLauncher is a DirectLauncher for seats: a new session of provider p is
// named "<p>-<n>", a resumed one keeps its id; during runs in the turn.
type seatLauncher struct {
	fakeLauncher
	mu     sync.Mutex
	runs   []LaunchSpec
	n      int
	err    error
	during func(spec LaunchSpec)
	// open: the seats' sessions are open in the agent's app (seatOccupied).
	open atomic.Bool
}

func (l *seatLauncher) Direct(string) bool { return true }

func (l *seatLauncher) busy(Seat, time.Time) bool { return l.open.Load() }

func (l *seatLauncher) set(err error, during func(LaunchSpec)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err, l.during = err, during
}

func (l *seatLauncher) Run(_ context.Context, spec LaunchSpec, started func(string)) error {
	l.mu.Lock()
	l.runs = append(l.runs, spec)
	id := spec.ResumeID
	if id == "" {
		l.n++
		id = fmt.Sprintf("%s-%d", spec.Provider, l.n)
	}
	during, err := l.during, l.err
	l.mu.Unlock()
	started(id)
	if during != nil {
		during(spec)
	}
	return err
}

func (l *seatLauncher) all() []LaunchSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.runs)
}

// seatNode is a running solo project node with folder dir and launcher l.
func seatNode(t *testing.T, dataDir, dir string, l *seatLauncher) *testNode {
	t.Helper()
	return seatNodeOf(t, newTestProject(t), dataDir, dir, l)
}

func seatNodeOf(t *testing.T, p testProject, dataDir, dir string, l *seatLauncher) *testNode {
	t.Helper()
	a := newProjectNodeAt(t, "morgott", p, dataDir, listen(t), nil)
	a.SetFolders(dir, nil)
	a.SetLauncher(l, ProviderClaude)
	a.wakeEvery = time.Hour // the tests run seatsDue themselves
	a.seatBusy = l.busy
	a.start(t)
	return a
}

func seatByLabel(t *testing.T, n *testNode, label string) SeatView {
	t.Helper()
	for _, s := range n.Seats() {
		if s.Label == label {
			return s
		}
	}
	t.Fatalf("no seat %q in %+v", label, n.Seats())
	return SeatView{}
}

func pendingIDs(s SeatView) []string {
	var out []string
	for _, p := range s.Pending {
		out = append(out, p.ID)
	}
	return out
}

// addSeats adds a Claude and a Codex seat and waits for their sessions.
func addSeats(t *testing.T, a *testNode) (claude, codex SeatView) {
	t.Helper()
	for _, p := range []string{ProviderClaude, ProviderCodex} {
		if _, err := a.AddSeat(SeatRequest{Provider: p}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "seat sessions", func() bool {
		for _, s := range a.Seats() {
			if s.SessionID == "" || s.Status == SeatRunning {
				return false
			}
		}
		return true
	})
	return seatByLabel(t, a, "Claude"), seatByLabel(t, a, "Codex")
}

func TestSeatAddPersistRemove(t *testing.T) {
	l := &seatLauncher{}
	data, dir, p := t.TempDir(), t.TempDir(), newTestProject(t)
	a := seatNodeOf(t, p, data, dir, l)
	if _, err := a.AddSeat(SeatRequest{Provider: "gpt"}); err == nil {
		t.Fatal("a seat of an unknown provider was added")
	}
	claude, codex := addSeats(t, a)
	if claude.Provider != ProviderClaude || codex.Provider != ProviderCodex || claude.Status != SeatOffline {
		t.Fatalf("seats %+v %+v", claude, codex)
	}
	runs := l.all()
	if len(runs) != 2 || runs[0].Seat == "" || !runs[0].NoOpen || runs[0].ResumeID != "" || runs[0].Folder != dir ||
		!strings.Contains(runs[0].Prompt, "agent-link: вы — агент «") || !slices.Contains(runs[0].Env, "AGENTLINK_SEAT="+runs[0].Seat) {
		t.Fatalf("intro turns %+v", runs)
	}
	// Labels stay unique: the next Claude is "Claude 2"; a taken label is refused.
	if s, err := a.AddSeat(SeatRequest{Provider: ProviderClaude}); err != nil || s.Label != "Claude 2" {
		t.Fatalf("second claude %+v %v", s, err)
	}
	if _, err := a.AddSeat(SeatRequest{Provider: ProviderCodex, Label: "codex"}); err == nil {
		t.Fatal("a taken label was accepted")
	}
	eventually(t, "third seat started", func() bool { return seatByLabel(t, a, "Claude 2").SessionID != "" })
	if err := a.RemoveSeat(seatByLabel(t, a, "Claude 2").ID); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveSeat("seat-none"); !errors.Is(err, ErrUnknownSeat) {
		t.Fatalf("remove unknown: %v", err)
	}
	// The seats outlive a restart, sessions included.
	a.stop()
	b := newProjectNodeAt(t, "morgott", p, data, listen(t), nil)
	got := b.Seats()
	if len(got) != 2 || got[0].SessionID != claude.SessionID || got[1].SessionID != codex.SessionID {
		t.Fatalf("after restart %+v", got)
	}
}

// A person asks the Codex seat: only it gets the message, and while its
// session is not live the node runs a turn of it resuming that session.
func TestSeatAskedRunsTurn(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	claude, codex := addSeats(t, a)
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "hi codex", AuthorKind: AuthorHuman, AskSeats: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.AskSeats, []string{codex.ID}) || m.Agent != nil {
		t.Fatalf("sent %+v", m)
	}
	if got := pendingIDs(seatByLabel(t, a, "Codex")); !slices.Equal(got, []string{m.ID}) || len(seatByLabel(t, a, "Claude").Pending) != 0 {
		t.Fatalf("pending codex %v claude %+v", got, seatByLabel(t, a, "Claude"))
	}
	// Not the person's own information for every session: it asks a seat.
	if p, _ := a.Unread("", "", 10); len(p.Messages) != 0 {
		t.Fatalf("unread for the node %+v", p.Messages)
	}
	if _, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "x", AuthorKind: AuthorHuman, AskSeats: []string{"nobody"}}); !errors.Is(err, ErrUnknownSeat) {
		t.Fatalf("unknown seat: %v", err)
	}
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "codex turn", func() bool { return len(seatByLabel(t, a, "Codex").Pending) == 0 })
	runs := l.all()
	last := runs[len(runs)-1]
	if last.Provider != ProviderCodex || last.ResumeID != codex.SessionID || !strings.Contains(last.Prompt, "hi codex") ||
		!strings.Contains(last.Prompt, "Просит ответа от вас") || strings.Contains(last.Prompt, "вы — агент") ||
		!slices.Contains(last.Env, "AGENTLINK_CHAT_ID="+chat.ID) {
		t.Fatalf("turn %+v", last)
	}
	if n := len(runs); n != 3 {
		t.Fatalf("turns %d: %+v", n, runs)
	}
	// The turn leased the seat's message; the turn that succeeded acked it.
	if l, ok := a.leases.get(leaseKey(codex.ID, m.ID)); !ok || l.State != LeaseAcked || l.Owner != seatOwner(codex.ID) || l.Via != ViaSeat || l.Attempts != 1 {
		t.Fatalf("seat lease %+v", l)
	}
	_ = claude
}

// A seat with a live session gets its messages through that session's hooks:
// unread, claim and ack for it alone; the node runs no turn of it.
func TestSeatLiveSessionRouting(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	claude, codex := addSeats(t, a)
	for _, s := range []SeatView{claude, codex} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: s.SessionID, Provider: s.Provider, Folder: dir}); err != nil {
			t.Fatal(err)
		}
	}
	if s := seatByLabel(t, a, "Codex"); s.Status != SeatActive {
		t.Fatalf("status %q", s.Status)
	}
	chat, _ := a.NewProjectChat(nil)
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "review this", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := a.UnreadFor(dir, claude.SessionID, "", 10); len(p.Messages) != 0 {
		t.Fatalf("claude sees %+v", p.Messages)
	}
	p, _ := a.UnreadFor(dir, codex.SessionID, "", 10)
	if len(p.Messages) != 1 || p.Messages[0].ID != m.ID || !p.Messages[0].AsksYou || p.Messages[0].ForSeat != codex.ID {
		t.Fatalf("codex sees %+v", p.Messages)
	}
	if !strings.Contains(FormatUnread(p.Messages[0]), "Просит ответа от вас") {
		t.Fatalf("format %q", FormatUnread(p.Messages[0]))
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: claude.SessionID}); len(g) != 0 {
		t.Fatalf("claude claimed %v", g)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: codex.SessionID}); !slices.Equal(g, []string{m.ID}) {
		t.Fatalf("codex claim %v", g)
	}
	a.seatsDue(context.Background(), time.Now())
	if n := len(l.all()); n != 2 {
		t.Fatalf("the node ran a live seat: %d turns", n)
	}
	res, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: codex.SessionID})
	if err != nil || len(res) != 1 || !res[0].WasUnread {
		t.Fatalf("ack %+v %v", res, err)
	}
	if len(seatByLabel(t, a, "Codex").Pending) != 0 {
		t.Fatal("still pending after the ack")
	}
}

// Seats ask each other: the author seat is named on the message, a reply goes
// back to the asking seat as information, and a chain of agents alone pauses
// past MaxAutoDepth (no turn runs for it).
func TestSeatConversationLoopLimit(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	claude, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	start, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "talk", AuthorKind: AuthorHuman, AskSeats: []string{claude.ID}})
	if err != nil {
		t.Fatal(err)
	}
	a.seatAck("", claude.ID, []string{start.ID})
	q, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "codex, what is 2+2?", ReplyTo: start.ID, Seat: claude.ID, AskSeats: []string{"Codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.Agent == nil || q.Agent.Seat != claude.ID || q.Agent.Provider != ProviderClaude || AuthorName(q) != "morgott · Claude" || q.AutoDepth != 1 {
		t.Fatalf("question %+v %+v", q, q.Agent)
	}
	ans, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "4", ReplyTo: q.ID, Seat: codex.ID})
	if err != nil {
		t.Fatal(err)
	}
	cs := seatByLabel(t, a, "Claude")
	if len(cs.Pending) != 1 || cs.Pending[0].ID != ans.ID || cs.Pending[0].Ask {
		t.Fatalf("the answer did not reach the asker as information: %+v", cs.Pending)
	}
	if ans.AutoDepth != 2 || ans.RootID != start.ID {
		t.Fatalf("chain %d %s", ans.AutoDepth, ans.RootID)
	}
	a.seatAck("", claude.ID, []string{ans.ID})
	a.seatAck("", codex.ID, []string{q.ID})
	// Ping-pong until the chain passes MaxAutoDepth.
	prev, from, to := ans, codex, claude
	for prev.AutoDepth <= MaxAutoDepth {
		from, to = to, from
		next, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "again", ReplyTo: prev.ID, Seat: from.ID, AskSeats: []string{to.ID}})
		if err != nil {
			t.Fatal(err)
		}
		a.seatAck("", from.ID, []string{prev.ID})
		prev = next
	}
	target := seatByLabel(t, a, to.Label)
	if len(target.Pending) != 1 || target.Pending[0].ID != prev.ID {
		t.Fatalf("pending %+v", target.Pending)
	}
	a.RegisterSession(SessionRequest{SessionID: target.SessionID, Provider: target.Provider, Folder: dir}) //nolint:errcheck // checked below
	p, _ := a.UnreadFor(dir, target.SessionID, "", 10)
	if len(p.Messages) != 1 || !p.Messages[0].Paused || p.Messages[0].AsksYou {
		t.Fatalf("past the limit: %+v", p.Messages)
	}
	if act, _ := a.unreadFor(dir, target.SessionID, "", 10, true); len(act.Messages) != 0 {
		t.Fatalf("a paused message wakes: %+v", act.Messages)
	}
	if err := a.EndSession(target.SessionID); err != nil {
		t.Fatal(err)
	}
	before := len(l.all())
	a.seatsDue(context.Background(), time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(l.all()); n != before {
		t.Fatalf("a turn ran for a paused chain: %d -> %d", before, n)
	}
}

// Stop holds a seat's messages and ends its turns; Start runs it again.
func TestSeatStopStart(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	_, codex := addSeats(t, a)
	if _, err := a.StopSeat(codex.ID); err != nil {
		t.Fatal(err)
	}
	chat, _ := a.NewProjectChat(nil)
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "wait", AuthorKind: AuthorHuman, AskSeats: []string{"all"}})
	if err != nil || len(m.AskSeats) != 2 {
		t.Fatalf("ask all %+v %v", m.AskSeats, err)
	}
	before := len(l.all())
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "claude answered", func() bool { return len(seatByLabel(t, a, "Claude").Pending) == 0 })
	if s := seatByLabel(t, a, "Codex"); s.Status != SeatStopped || len(s.Pending) != 1 || len(l.all()) != before+1 {
		t.Fatalf("stopped seat %+v runs %d", s, len(l.all()))
	}
	l.mu.Lock()
	l.err = errors.New("boom")
	l.mu.Unlock()
	if _, err := a.StartSeat(codex.ID, false); err != nil {
		t.Fatal(err)
	}
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "failed turn reported", func() bool { return seatByLabel(t, a, "Codex").Error != "" })
	if s := seatByLabel(t, a, "Codex"); len(s.Pending) != 1 {
		t.Fatalf("a failed turn dropped the message: %+v", s)
	}
	n := len(l.all())
	a.seatsDue(context.Background(), time.Now())
	time.Sleep(100 * time.Millisecond)
	if len(l.all()) != n {
		t.Fatal("a failed seat ran again without a new message")
	}
}

// A seat's environment reaches Codex's commands through its config: Codex
// filters the environment it gives them.
func TestCodexServerArgs(t *testing.T) {
	if got := CodexServerArgs(LaunchSpec{}); !slices.Equal(got, []string{"app-server"}) {
		t.Fatalf("plain %q", got)
	}
	got := CodexServerArgs(LaunchSpec{Env: []string{"AGENTLINK_SEAT=seat-1", `PATH=C:\bin;C:\x`, "BAD=it's", "noequals"}})
	want := []string{"app-server", "-c", `shell_environment_policy.set={AGENTLINK_SEAT='seat-1',PATH='C:\bin;C:\x'}`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

// A seat whose session is open in the agent's app is not resumed (two
// writers of one session): it waits as busy, and runs once the app left it.
func TestSeatBusySessionNotResumed(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	if _, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "hi", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}}); err != nil {
		t.Fatal(err)
	}
	l.open.Store(true)
	before := len(l.all())
	a.seatsDue(context.Background(), time.Now())
	time.Sleep(100 * time.Millisecond)
	if s := seatByLabel(t, a, "Codex"); len(l.all()) != before || s.Status != SeatBusy || len(s.Pending) != 1 {
		t.Fatalf("an open session was resumed: runs %d -> %d, %+v", before, len(l.all()), s)
	}
	l.open.Store(false)
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "the turn after the app left it", func() bool { return len(seatByLabel(t, a, "Codex").Pending) == 0 })
	if s := seatByLabel(t, a, "Codex"); s.Status != SeatOffline || s.LastTurn.IsZero() {
		t.Fatalf("after the turn %+v", s)
	}
}

// sessionOccupied: the session's own transcript, written after the node's
// last turn of it and lately.
func TestSessionOccupied(t *testing.T) {
	claudeHome, codexHome, dir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now()
	sid := "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cp := filepath.Join(claudeHome, "projects", claudeProjectDir(filepath.Clean(dir)), sid+".jsonl")
	write(cp)
	if !sessionOccupied(ProviderClaude, claudeHome, codexHome, dir, sid, now.Add(-time.Minute), now) {
		t.Fatal("a transcript written after the last turn is not occupied")
	}
	if sessionOccupied(ProviderClaude, claudeHome, codexHome, dir, sid, now.Add(time.Minute), now) {
		t.Fatal("the node's own turn counts as another writer")
	}
	if sessionOccupied(ProviderClaude, claudeHome, codexHome, dir, "other", time.Time{}, now) || sessionOccupied(ProviderCodex, claudeHome, codexHome, dir, sid, time.Time{}, now) {
		t.Fatal("another session or provider is occupied")
	}
	if err := os.Chtimes(cp, now.Add(-2*occupiedWithin), now.Add(-2*occupiedWithin)); err != nil {
		t.Fatal(err)
	}
	if sessionOccupied(ProviderClaude, claudeHome, codexHome, dir, sid, time.Time{}, now) {
		t.Fatal("an old transcript is occupied")
	}
	// A resumed Codex thread writes to the rollout of its first day.
	write(filepath.Join(codexHome, "sessions", "2026", "01", "02", "rollout-2026-01-02T10-00-00-"+sid+".jsonl"))
	if !sessionOccupied(ProviderCodex, claudeHome, codexHome, dir, sid, time.Time{}, now) {
		t.Fatal("an old day's rollout written now is not occupied")
	}
}

// The node's turn claims its messages: a hook of the seat's session that
// registers meanwhile neither sees nor takes them; a failed turn frees them.
func TestSeatTurnClaimsItsMessages(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	_, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "review", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var granted []string
	seen := -1
	l.set(errors.New("boom"), func(spec LaunchSpec) {
		if spec.Seat != codex.ID {
			return
		}
		if _, err := a.RegisterSession(SessionRequest{SessionID: codex.SessionID, Provider: ProviderCodex, Folder: dir}); err != nil {
			t.Error(err)
		}
		granted, _ = a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: codex.SessionID})
		p, _ := a.UnreadFor(dir, codex.SessionID, "", 10)
		seen = len(p.Messages)
		if err := a.EndSession(codex.SessionID); err != nil {
			t.Error(err)
		}
	})
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "the failed turn", func() bool { s := seatByLabel(t, a, "Codex"); return s.Fails == 1 && s.Status != SeatRunning })
	if len(granted) != 0 || seen != 0 {
		t.Fatalf("a hook took the turn's message: granted %v, unread %d", granted, seen)
	}
	l.set(nil, nil)
	if _, err := a.RegisterSession(SessionRequest{SessionID: codex.SessionID, Provider: ProviderCodex, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: codex.SessionID}); !slices.Equal(g, []string{m.ID}) {
		t.Fatalf("the failed turn kept its claim: %v", g)
	}
}

// A peer's reply to a seat's message goes to that seat, asked when it asks
// this node, and to it alone: not the worker's, not unread for the others.
func TestSeatGetsPeerReply(t *testing.T) {
	p := newTestProject(t)
	lnA, lnB := listen(t), listen(t)
	l := &seatLauncher{}
	a := newProjectNode(t, "a", p, lnA, map[string]net.Listener{"b": lnB})
	a.SetFolders(t.TempDir(), nil)
	a.SetLauncher(l, ProviderClaude)
	a.wakeEvery, a.seatBusy = time.Hour, l.busy
	b := newProjectNode(t, "b", p, lnB, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	_, codex := addSeats(t, a)
	chat, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "b, which port?", Ask: []string{"b"}, Seat: codex.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the question", func() bool { return slices.Contains(chatIDs(b, chat.ID), q.ID) })
	ans, err := b.SendChat(ChatSend{ChatID: chat.ID, Body: "8080; why?", ReplyTo: q.ID, Ask: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the seat has the reply", func() bool { return slices.Contains(pendingIDs(seatByLabel(t, a, "Codex")), ans.ID) })
	if s := seatByLabel(t, a, "Codex"); !s.Pending[len(s.Pending)-1].Ask {
		t.Fatalf("a reply that asks this node does not ask the seat: %+v", s.Pending)
	}
	rec, _ := a.chats.message(ans.ID)
	if rec.Assigned != "seat:"+codex.ID {
		t.Fatalf("assigned %q", rec.Assigned)
	}
	if run, _, _ := a.ClaimRun(rec.Message); run {
		t.Fatal("the worker takes the seat's message")
	}
	if u, _ := a.Unread("", "", 10); slices.Contains(ids(u.Messages), ans.ID) {
		t.Fatal("the seat's message is unread for every session")
	}
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "the seat's turn", func() bool { return len(seatByLabel(t, a, "Codex").Pending) == 0 })
	if last := l.all()[len(l.all())-1]; !strings.Contains(last.Prompt, "8080; why?") {
		t.Fatalf("turn %+v", last)
	}
}

// Seats asking each other without --reply-to continue the chain of the
// message they were asked by: past MaxAutoDepth the ask pauses, and a turn
// leaves a paused message out of its prompt.
func TestSeatAsksWithoutReplyToCountDepth(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	claude, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	start, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "talk", AuthorKind: AuthorHuman, AskSeats: []string{claude.ID}})
	if err != nil {
		t.Fatal(err)
	}
	prev, from, to := start, codex, claude
	for prev.AutoDepth <= MaxAutoDepth {
		from, to = to, from
		next, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: fmt.Sprintf("hop %d", prev.AutoDepth+1), Seat: from.ID, AskSeats: []string{to.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if next.AutoDepth != prev.AutoDepth+1 || next.RootID != start.ID {
			t.Fatalf("hop after %d: depth %d root %s", prev.AutoDepth, next.AutoDepth, next.RootID)
		}
		prev = next
	}
	paused := prev
	fresh, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "fresh question", AuthorKind: AuthorHuman, AskSeats: []string{to.ID}})
	if err != nil {
		t.Fatal(err)
	}
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "the turn", func() bool {
		s := seatByLabel(t, a, to.Label)
		return !slices.Contains(pendingIDs(s), fresh.ID) && s.Status != SeatRunning
	})
	var last LaunchSpec
	for _, r := range l.all() {
		if r.Seat == to.ID {
			last = r
		}
	}
	if !strings.Contains(last.Prompt, "fresh question") || strings.Contains(last.Prompt, paused.Body) {
		t.Fatalf("prompt %q", last.Prompt)
	}
	if !slices.Contains(pendingIDs(seatByLabel(t, a, to.Label)), paused.ID) {
		t.Fatal("the paused message left without a person")
	}
}

// A turn never starts for a seat stopped or removed after it was due.
func TestSeatTurnSkipsStoppedOrRemoved(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	if _, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "x", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}}); err != nil {
		t.Fatal(err)
	}
	before := len(l.all())
	if _, err := a.StopSeat(codex.ID); err != nil {
		t.Fatal(err)
	}
	a.seatTurn(context.Background(), l, codex.ID, false, false)
	if err := a.RemoveSeat(codex.ID); err != nil {
		t.Fatal(err)
	}
	a.seatTurn(context.Background(), l, codex.ID, false, false)
	if n := len(l.all()); n != before {
		t.Fatalf("turns ran: %d -> %d", before, n)
	}
	if _, ok := a.claimTurn(context.Background(), codex.ID, nil, false); ok {
		t.Fatal("a removed seat's turn may start")
	}
}

// A failed turn is tried again after each of seatRetry, then waits for a
// person (needs_human) until a new message.
func TestSeatRetriesFailedTurn(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	l.set(errors.New("transient"), nil)
	if _, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "x", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fails := func(n int) {
		t.Helper()
		eventually(t, fmt.Sprintf("failure %d", n), func() bool { s := seatByLabel(t, a, "Codex"); return s.Fails == n && s.Status != SeatRunning })
	}
	base := len(l.all())
	a.seatsDue(ctx, time.Now())
	fails(1)
	s := seatByLabel(t, a, "Codex")
	if d := s.RetryAt.Sub(s.LastTurn); d < seatRetry[0]-time.Second || d > seatRetry[0]+time.Second {
		t.Fatalf("first retry after %v", d)
	}
	a.seatsDue(ctx, time.Now().Add(seatRetry[0]/2))
	time.Sleep(100 * time.Millisecond)
	if n := len(l.all()); n != base+1 {
		t.Fatalf("retried before its time: %d runs", n-base)
	}
	for i := range seatRetry {
		a.seatsDue(ctx, seatByLabel(t, a, "Codex").RetryAt.Add(time.Second))
		fails(i + 2)
	}
	if s := seatByLabel(t, a, "Codex"); s.Status != SeatNeedsHuman || len(s.Pending) != 1 || s.Error == "" {
		t.Fatalf("after the retries %+v", s)
	}
	a.seatsDue(ctx, time.Now().Add(24*time.Hour))
	time.Sleep(100 * time.Millisecond)
	if n := len(l.all()); n != base+1+len(seatRetry) {
		t.Fatalf("ran past its retries: %d runs", n-base)
	}
	l.set(nil, nil)
	if _, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "again", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}}); err != nil {
		t.Fatal(err)
	}
	a.seatsDue(ctx, time.Now())
	eventually(t, "answered after a new message", func() bool { return len(seatByLabel(t, a, "Codex").Pending) == 0 })
}

// A send whose seats' queue cannot be saved reports it, and the queue reaches
// the disk later.
func TestSeatQueueSaveFailure(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	chat, _ := a.NewProjectChat(nil)
	st := a.seats
	st.mu.Lock()
	good := st.path
	st.path = filepath.Join(t.TempDir(), "missing", "seats.json")
	st.mu.Unlock()
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "x", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}})
	if err == nil || m.ID == "" {
		t.Fatalf("send %+v %v", m, err)
	}
	st.mu.Lock()
	st.path = good
	st.mu.Unlock()
	l.open.Store(true) // no turn: the message stays pending
	a.seatsDue(context.Background(), time.Now())
	var saved []*Seat
	if err := readJSON(good, &saved); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(saved, func(s *Seat) bool { return s.ID == codex.ID })
	if i < 0 || !slices.ContainsFunc(saved[i].Pending, func(p SeatPending) bool { return p.ID == m.ID }) {
		t.Fatalf("not saved: %+v", saved)
	}
}

// The setup turn of a new seat posts nothing; a turn with messages may.
func TestSeatSetupTurnPostsNothing(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	var setupErr, askErr error
	var setupPrompt string
	chatOf := func(spec LaunchSpec) string {
		for _, e := range spec.Env {
			if v, ok := strings.CutPrefix(e, "AGENTLINK_CHAT_ID="); ok {
				return v
			}
		}
		return ""
	}
	l.set(nil, func(spec LaunchSpec) {
		if strings.Contains(spec.Prompt, "question") {
			_, askErr = a.SendRequest(SendRequest{ChatID: chatOf(spec), Body: "answer", Seat: spec.Seat})
			return
		}
		setupPrompt = spec.Prompt
		_, setupErr = a.SendRequest(SendRequest{ChatID: chatOf(spec), Body: "hi Codex", Seat: spec.Seat})
	})
	if _, err := a.AddSeat(SeatRequest{Provider: ProviderClaude}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "setup turn", func() bool { s := seatByLabel(t, a, "Claude"); return s.SessionID != "" && s.Status != SeatRunning })
	if !errors.Is(setupErr, ErrSeatSetup) || !strings.Contains(setupPrompt, "ничего не отправляйте") {
		t.Fatalf("setup turn sent: %v; prompt %q", setupErr, setupPrompt)
	}
	claude := seatByLabel(t, a, "Claude")
	chat, _ := a.NewProjectChat(nil)
	if _, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "a question", AuthorKind: AuthorHuman, AskSeats: []string{claude.ID}}); err != nil {
		t.Fatal(err)
	}
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "answered", func() bool { return len(seatByLabel(t, a, "Claude").Pending) == 0 })
	if askErr != nil {
		t.Fatalf("a turn with a message may not send: %v", askErr)
	}
}

// A seat's message whose lease failed is not run in a turn (like a launch's):
// no turn starts for it, it stays pending for a person.
func TestSeatFailedLeaseNotRun(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "hi codex", AuthorKind: AuthorHuman, AskSeats: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := a.leases.take(m.ID, codex.ID, seatOwner(codex.ID), ViaSeat, "", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := a.leases.fail(seatOwner(codex.ID), []string{m.ID}, "seat_turn_failed", now); err != nil {
		t.Fatal(err)
	}
	before := len(l.all())
	a.seatsDue(context.Background(), time.Now())
	a.seatTurn(context.Background(), l, codex.ID, false, false)
	if runs := len(l.all()); runs != before || len(seatByLabel(t, a, "Codex").Pending) != 1 {
		t.Fatalf("runs %d -> %d, pending %+v", before, runs, seatByLabel(t, a, "Codex").Pending)
	}
}
