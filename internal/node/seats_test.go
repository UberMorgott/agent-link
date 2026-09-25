package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
}

func (l *seatLauncher) Direct(string) bool { return true }

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
