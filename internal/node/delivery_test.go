package node

import (
	"bytes"
	"context"
	"errors"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePoster records inbox posts.
type fakePoster struct {
	mu    sync.Mutex
	err   error
	calls []string // "socket|token|text"
}

func (p *fakePoster) Post(_ context.Context, socket, token, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, socket+"|"+token+"|"+text)
	return p.err
}

func (p *fakePoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// fakeLauncher records launches.
type fakeLauncher struct {
	mu    sync.Mutex
	err   error
	specs []LaunchSpec
}

func (l *fakeLauncher) Launch(_ context.Context, spec LaunchSpec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, spec)
	return l.err
}

func (l *fakeLauncher) all() []LaunchSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.specs)
}

const (
	testSocket = `\\.\pipe\LOCAL\cc-msg-0123456789abcdef0123456789abcdef`
	testToken  = "c4a21bf1a50e979413fbed22a580a6bf" //nolint:gosec // G101: a made-up inbox token for tests
)

// deliveryPair is a and b with a's working folder dir, poster p and launcher
// l (either may be nil); the test drives the ladder itself.
func deliveryPair(t *testing.T, dir string, p InboxPoster, l SessionLauncher) (*testNode, *testNode) {
	t.Helper()
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, []string{"dev"}, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	a.SetFolders(dir, nil)
	if p != nil {
		a.SetInboxPoster(p)
	}
	if l != nil {
		a.SetLauncher(l, "")
		a.SetAutonomy(Autonomy{Mode: AutonomyAsked, MaxDepth: -1})
	}
	a.wakeEvery = time.Hour
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	return a, b
}

// ask sends a request from b to a and waits until a has it unread.
func ask(t *testing.T, a, b *testNode, body string) Message {
	t.Helper()
	m, err := b.SendRequest(SendRequest{To: "a", Body: body, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has "+body, func() bool {
		p, _ := a.Unread("", "", 100)
		return slices.ContainsFunc(p.Messages, func(u UnreadMessage) bool { return u.ID == m.ID })
	})
	return m
}

// attemptsOf returns the attempt events b sees on its message m for a.
func attemptsOf(b *testNode, m Message) []string {
	msgs, err := b.ChatMessages(m.ChatID, 0, 0, 100)
	if err != nil {
		return nil
	}
	for _, cm := range msgs {
		if cm.ID != m.ID {
			continue
		}
		for _, d := range cm.Delivery {
			if d.Peer == "a" {
				var out []string
				for _, at := range d.Attempts {
					out = append(out, at.Event)
				}
				return out
			}
		}
	}
	return nil
}

func waitAttempts(t *testing.T, b *testNode, m Message, want ...string) {
	t.Helper()
	eventually(t, "attempts "+strings.Join(want, ","), func() bool { return slices.Equal(attemptsOf(b, m), want) })
}

func TestWriteInbox(t *testing.T) {
	var buf bytes.Buffer
	if err := writeInbox(&buf, "tok", `hi "there"`); err != nil {
		t.Fatal(err)
	}
	want := `{"token":"tok","type":"auth"}` + "\n" + `{"message":{"content":"hi \"there\"","role":"user"},"type":"user"}` + "\n"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
	if err := writeInbox(&buf, "", "x"); err == nil {
		t.Fatal("no token accepted")
	}
}

func TestValidInbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		for s, ok := range map[string]bool{
			testSocket: true, `\\.\pipe\LOCAL\cc-msg-4f99f28c24f33ee84c6dd8f59791167e`: true,
			`\\.\pipe\other`: false, `C:\x\cc-msg-00000000`: false, `\\.\pipe\LOCAL\cc-msg-zz`: false, "": false,
		} {
			if validInboxSocket(s) != ok {
				t.Errorf("validInboxSocket(%q) != %v", s, ok)
			}
		}
	}
	if !validInboxToken(testToken) || validInboxToken("short") || validInboxToken("has space in it 0123456789") {
		t.Error("validInboxToken")
	}
}

// An idle Claude session with an inbox is woken there once per idle period;
// the author sees wake_requested, then woken_confirmed when it reads. A
// failed post drops the inbox (the waiter takes over).
func TestWakeIdleClaudeInbox(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	reg := func(idle bool, sock, tok string) {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: "s-c", Provider: "claude", Folder: dir, Wake: WakeRewake,
			Idle: idle, InboxSocket: sock, InboxToken: tok}); err != nil {
			t.Fatal(err)
		}
	}
	reg(false, testSocket, testToken)
	if !a.InboxWakes("s-c") {
		t.Fatal("inbox not kept")
	}
	if s := a.Sessions(); len(s) != 1 || !s[0].Inbox {
		t.Fatalf("sessions %+v", s)
	}
	m := ask(t, a, b, "wake up")
	a.wakeIdle(context.Background())
	if p.count() != 0 {
		t.Fatal("a busy session was woken")
	}
	reg(true, testSocket, testToken)
	a.wakeIdle(context.Background())
	a.wakeIdle(context.Background())
	if p.count() != 1 || !strings.HasPrefix(p.calls[0], testSocket+"|"+testToken+"|agent-link: новые сообщения (1).") ||
		!strings.Contains(p.calls[0], "wake up") || !strings.Contains(p.calls[0], "id "+m.ID) {
		t.Fatalf("posts %v", p.calls)
	}
	// The prompt carries it: the session's hooks do not get it again.
	if page, _ := a.UnreadFor(dir, "s-c", "", 10); page.Total != 0 || len(page.Woken) != 1 || page.Woken[0].ID != m.ID {
		t.Fatalf("after the wake: %+v", page)
	}
	waitAttempts(t, b, m, AttemptWakeRequested)
	if !a.InboxWakes("s-c") {
		t.Fatal("waiter not held off right after the wake")
	}
	// Still idle long after the wake (the post was dropped): the waiter takes over.
	a.sess.mu.Lock()
	ia := a.sess.inbox["s-c"]
	ia.woke = time.Now().Add(-inboxWakeGrace - time.Second)
	a.sess.inbox["s-c"] = ia
	a.sess.mu.Unlock()
	reg(true, testSocket, testToken) // a heartbeat keeps the wake time
	if a.InboxWakes("s-c") {
		t.Fatal("waiter still held off after an ignored wake")
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-c"}); err != nil {
		t.Fatal(err)
	}
	waitAttempts(t, b, m, AttemptWakeRequested, AttemptWokenConfirmed)

	// A failed post: the inbox is dropped, and the session is not woken again.
	p.mu.Lock()
	p.err = errors.New("pipe gone")
	p.mu.Unlock()
	ask(t, a, b, "again")
	reg(false, testSocket, testToken)
	reg(true, testSocket, testToken)
	a.wakeIdle(context.Background())
	if a.InboxWakes("s-c") || p.count() != 2 {
		t.Fatalf("after a failed post: inbox %v posts %d", a.InboxWakes("s-c"), p.count())
	}
	// Its claim is dropped: the hooks deliver it.
	if page, _ := a.UnreadFor(dir, "s-c", "", 10); page.Total != 1 || len(page.Woken) != 0 {
		t.Fatalf("after a failed post: %+v", page)
	}
	// An invalid inbox is ignored, not refused.
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-x", Provider: "claude", Folder: dir, InboxSocket: `C:\evil`, InboxToken: testToken}); err != nil || a.InboxWakes("s-x") {
		t.Fatalf("invalid inbox: %v %v", err, a.InboxWakes("s-x"))
	}
}

func TestLaunchCommand(t *testing.T) {
	for _, c := range []struct {
		spec LaunchSpec
		want string
	}{
		{LaunchSpec{Provider: ProviderClaude, Folder: `C:\p`, Prompt: "read; now"}, `wt.exe -w new -d C:\p claude --permission-mode bypassPermissions read now`},
		{LaunchSpec{Provider: ProviderClaude, Folder: `C:\p`, ResumeID: "abc", Prompt: "go"}, `wt.exe -w new -d C:\p claude --permission-mode bypassPermissions --resume abc go`},
		{LaunchSpec{Provider: ProviderCodex, Folder: `C:\p`, Prompt: "go"}, `wt.exe -w new -d C:\p codex --dangerously-bypass-approvals-and-sandbox -C C:\p go`},
		{LaunchSpec{Provider: ProviderCodex, Folder: `C:\p`, ResumeID: "t-1", Prompt: "go"}, `wt.exe -w new -d C:\p codex resume --dangerously-bypass-approvals-and-sandbox t-1 go`},
	} {
		got := LaunchCommand(c.spec)
		if strings.Join(got, " ") != c.want {
			t.Errorf("%+v: %q", c.spec, got)
		}
		if last := got[len(got)-1]; strings.ContainsAny(last, `;"`) {
			t.Errorf("unsafe prompt %q", last)
		}
	}
	if got := launchSafe("a;b \"c\"\n& d"); got != "a b c d" {
		t.Errorf("launchSafe %q", got)
	}
	env := LaunchEnv([]string{"PATH=x", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "claude_code_messaging_token=t",
		"CODEX_HOME=h", "CODEX_THREAD_ID=1", "AGENTLINK_JOB_ID=j", "CLAUDE_PID=3", "ANTHROPIC_BASE_URL=u"})
	if !slices.Equal(env, []string{"PATH=x", "CODEX_HOME=h", "ANTHROPIC_BASE_URL=u"}) {
		t.Errorf("LaunchEnv %q", env)
	}
}

// No live session: the node opens one (after the grace), once; the session's
// start confirms it. Every step reaches the author.
func TestLaunchLadderConfirmed(t *testing.T) {
	l := &fakeLauncher{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	m := ask(t, a, b, "please look")
	ctx := context.Background()
	a.launchDue(ctx, time.Now())
	if len(l.all()) != 0 {
		t.Fatal("launched within the grace period")
	}
	later := time.Now().Add(launchGrace + time.Second)
	a.launchDue(ctx, later)
	a.launchDue(ctx, later.Add(time.Second)) // pending: no second launch
	specs := l.all()
	if len(specs) != 1 || specs[0].Provider != ProviderClaude || specs[0].Folder != dir || specs[0].ResumeID != "" ||
		!strings.Contains(specs[0].Prompt, "(1)") || !strings.Contains(specs[0].Prompt, "please look") || !strings.Contains(specs[0].Prompt, "от b") {
		t.Fatalf("launches %+v", specs)
	}
	waitAttempts(t, b, m, AttemptLaunchRequested)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-new", Provider: "codex", Folder: dir}); err != nil {
		t.Fatal(err)
	}
	a.launchDue(ctx, later.Add(2*time.Second))
	waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchConfirmed)

	// The next launch is a new session (never a resume), in the agent of the
	// folder's last session.
	if err := a.EndSession("s-new"); err != nil {
		t.Fatal(err)
	}
	ask(t, a, b, "more")
	next := later.Add(launchDebounce + launchGrace + time.Minute)
	a.launchDue(ctx, next)
	if specs := l.all(); len(specs) != 2 || specs[1].Provider != ProviderCodex || specs[1].ResumeID != "" ||
		!strings.Contains(specs[1].Prompt, "(1)") { // the confirmed launch's message is not launched for again
		t.Fatalf("resume %+v", specs)
	}
}

// An opened session that never starts is retried once as a new session, then
// given up: launch_failed:timeout, and that message is not launched for again.
func TestLaunchLadderTimeout(t *testing.T) {
	l := &fakeLauncher{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "old", Provider: "claude", Folder: dir}); err != nil {
		t.Fatal(err)
	}
	if err := a.EndSession("old"); err != nil {
		t.Fatal(err)
	}
	m := ask(t, a, b, "hello")
	ctx := context.Background()
	t0 := time.Now().Add(launchGrace + time.Second)
	a.launchDue(ctx, t0)
	a.launchDue(ctx, t0.Add(launchConfirm+time.Second))
	a.launchDue(ctx, t0.Add(launchConfirm+2*time.Second)) // pending again
	specs := l.all()
	if len(specs) != 2 || specs[0].ResumeID != "" || specs[1].ResumeID != "" {
		t.Fatalf("tries %+v", specs)
	}
	a.launchDue(ctx, t0.Add(2*launchConfirm+3*time.Second))
	waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchFailed+":timeout")
	a.launchDue(ctx, t0.Add(launchDebounce+3*launchConfirm))
	if len(l.all()) != 2 {
		t.Fatalf("a failed message launched again: %+v", l.all())
	}
}

// Eligibility is re-read before every attempt: a message read meanwhile, or
// paused by the loop guard, opens nothing (a paused one needs a person); a
// launcher error is reported at once.
func TestLaunchLadderEligibility(t *testing.T) {
	l := &fakeLauncher{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	ctx := context.Background()
	m := ask(t, a, b, "read elsewhere")
	if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}}); err != nil {
		t.Fatal(err)
	}
	a.launchDue(ctx, time.Now().Add(launchGrace+time.Second))
	if len(l.all()) != 0 {
		t.Fatal("launched for a read message")
	}

	chat, err := b.CreateChat([]string{"a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a knows chat", func() bool { _, ok := a.ChatOf(chat.ID); return ok })
	deep, err := b.SendMessage(Message{ChatID: chat.ID, Body: "guarded", Responders: []string{"a"}, RootID: newID(), AutoDepth: MaxAutoDepth + 1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the paused message", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	a.launchDue(ctx, time.Now().Add(launchGrace+time.Second))
	if len(l.all()) != 0 {
		t.Fatal("launched for a paused message")
	}
	waitAttempts(t, b, deep, AttemptNeedsHuman)

	// Off: nothing opens.
	a.SetAutonomy(Autonomy{Mode: AutonomyOff, MaxDepth: -1})
	m2 := ask(t, a, b, "no agent here")
	a.launchDue(ctx, time.Now().Add(launchGrace+time.Second))
	if len(l.all()) != 0 {
		t.Fatal("launched with auto-open off")
	}
	a.SetAutonomy(Autonomy{Mode: AutonomyAsked, MaxDepth: -1})
	l.mu.Lock()
	l.err = ErrNoAgent
	l.mu.Unlock()
	a.launchDue(ctx, time.Now().Add(launchGrace+time.Second))
	waitAttempts(t, b, m2, AttemptLaunchRequested, AttemptLaunchFailed+":no_agent")
}

func TestAttemptEvents(t *testing.T) {
	for e, ok := range map[string]bool{
		AttemptWakeRequested: true, AttemptNeedsHuman: true, "launch_failed:timeout": true, "launch_failed:no_agent": true,
		"launch_failed:": false, "launch_failed:Bad Reason": false, "launch_failed": false, "other": false,
		"launch_failed:" + strings.Repeat("x", maxAttemptReason+1): false,
	} {
		if validAttemptEvent(e) != ok {
			t.Errorf("validAttemptEvent(%q) != %v", e, ok)
		}
	}
}

// A message keeps the latest maxAttemptsKept events per recipient; invalid
// events and events for messages of other authors are ignored.
func TestReceiveAttempts(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	m := ask(t, a, b, "x")
	c, _ := b.chats.get(m.ChatID)
	base := time.Now().UTC()
	env := Message{ChatID: m.ChatID, Kind: KindReceipt, CreatedAt: base}
	c.stamp(&env)
	for i := range maxAttemptsKept + 3 {
		env.Attempts = append(env.Attempts, Attempt{ID: m.ID, Event: AttemptWakeRequested, At: base.Add(time.Duration(i) * time.Second)})
	}
	env.Attempts = append(env.Attempts, Attempt{ID: m.ID, Event: "bogus", At: base}, Attempt{ID: newID(), Event: AttemptNeedsHuman, At: base})
	b.receiveReceipts("a", env)
	got := attemptsOf(b, m)
	if len(got) != maxAttemptsKept {
		t.Fatalf("kept %v", got)
	}
	msgs, _ := b.ChatMessages(m.ChatID, 0, 0, 10)
	i := slices.IndexFunc(msgs, func(cm ChatMessage) bool { return cm.ID == m.ID })
	if d := msgs[i].Delivery[0]; d.Attempt != AttemptWakeRequested || !d.Attempts[len(d.Attempts)-1].At.Equal(base.Add(time.Duration(maxAttemptsKept+2)*time.Second)) {
		t.Fatalf("delivery %+v", d)
	}
}

// One recipient per message: no idle session is woken while a session of the
// area is in a turn (its hooks deliver); then only the idle one seen last.
func TestWakeIdlePriority(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	const (
		sockOld = `\\.\pipe\LOCAL\cc-msg-4f99f28c24f33ee84c6dd8f59791167e`
		sockNew = `\\.\pipe\LOCAL\cc-msg-0123456789abcdef0123456789abcde0`
	)
	reg := func(id, sock string, idle bool) {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: "claude", Folder: dir, Wake: WakeRewake,
			Idle: idle, InboxSocket: sock, InboxToken: testToken}); err != nil {
			t.Fatal(err)
		}
	}
	reg("s-act", testSocket, false)
	reg("s-old", sockOld, true)
	reg("s-new", sockNew, true)
	a.sess.mu.Lock()
	a.sess.sessions["s-old"].LastActive = time.Now().Add(-time.Minute)
	a.sess.mu.Unlock()
	m := ask(t, a, b, "who takes it")
	a.wakeIdle(context.Background())
	if p.count() != 0 {
		t.Fatalf("an idle session woken while one is in a turn: %v", p.calls)
	}
	if err := a.EndSession("s-act"); err != nil {
		t.Fatal(err)
	}
	a.wakeIdle(context.Background())
	a.wakeIdle(context.Background())
	if p.count() != 1 || !strings.HasPrefix(p.calls[0], sockNew+"|") || !strings.Contains(p.calls[0], "id "+m.ID) {
		t.Fatalf("posts %v", p.calls)
	}
}

// Active wins over chat affinity: the chat's session is idle while another
// session of the folder is in a turn, so the message is the active one's
// (its hooks take it) and the idle one is not woken; once no session is in a
// turn, affinity names the idle one again and it is woken.
func TestWakeIdleAffinityActiveWins(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	reg := func(id, sock string, idle bool) {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: "claude", Folder: dir, Wake: WakeRewake,
			Idle: idle, InboxSocket: sock, InboxToken: testToken}); err != nil {
			t.Fatal(err)
		}
	}
	reg("s-idle", testSocket, true)
	reg("s-act", `\\.\pipe\LOCAL\cc-msg-4f99f28c24f33ee84c6dd8f59791167e`, false)
	q, err := a.SendRequest(SendRequest{To: "b", Body: "check it", AuthorKind: AuthorAgent, SessionID: "s-idle"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })
	m, err := b.SendRequest(SendRequest{ChatID: q.ChatID, Body: "new topic", Ask: []string{"a"}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has it", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	if to := a.routedTo([]string{m.ID})[m.ID]; to != "" {
		t.Fatalf("routed to %q while a session is in a turn", to)
	}
	a.wakeIdle(context.Background())
	if p.count() != 0 {
		t.Fatalf("the idle chat session was woken while one is active: %v", p.calls)
	}
	if page, _ := a.UnreadFor(dir, "s-act", "", 10); page.Total != 1 {
		t.Fatalf("the active session does not see it: %+v", page)
	}
	a.sess.mu.Lock()
	a.sess.sessions["s-act"].Idle = true
	a.sess.mu.Unlock()
	if to := a.routedTo([]string{m.ID})[m.ID]; to != "s-idle" {
		t.Fatalf("both idle: routed to %q, want the chat's session", to)
	}
	a.wakeIdle(context.Background())
	if p.count() != 1 || !strings.HasPrefix(p.calls[0], testSocket+"|") {
		t.Fatalf("posts %v", p.calls)
	}
}

// An abandoned session keeps living on its waiter's heartbeats, but those are
// no activity: after AffinityLapse without a hook event it loses its chats,
// and a message of them goes to the session of the folder active last.
func TestAffinityLapsesWithoutEvents(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	const sockNew = `\\.\pipe\LOCAL\cc-msg-0123456789abcdef0123456789abcde0`
	reg := func(id, sock string, heartbeat bool) {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: "claude", Folder: dir, Wake: WakeRewake,
			Idle: true, InboxSocket: sock, InboxToken: testToken, Heartbeat: heartbeat}); err != nil {
			t.Fatal(err)
		}
	}
	reg("s-old", testSocket, false)
	q, err := a.SendRequest(SendRequest{To: "b", Body: "check it", AuthorKind: AuthorAgent, SessionID: "s-old"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })
	reg("s-new", sockNew, false)
	m, err := b.SendRequest(SendRequest{ChatID: q.ChatID, Body: "done?", Ask: []string{"a"}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has it", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	if to := a.routedTo([]string{m.ID})[m.ID]; to != "s-old" {
		t.Fatalf("routed to %q, want the chat's session", to)
	}
	a.sess.mu.Lock()
	a.sess.sessions["s-old"].LastActive = time.Now().Add(-AffinityLapse - time.Minute)
	a.sess.mu.Unlock()
	reg("s-old", testSocket, true) // the waiter's heartbeat: live, not active
	a.sess.mu.Lock()
	old := *a.sess.sessions["s-old"]
	a.sess.mu.Unlock()
	if !old.live(time.Now()) || time.Since(old.LastActive) < AffinityLapse {
		t.Fatalf("heartbeat: live %v, last active %v", old.live(time.Now()), old.LastActive)
	}
	if to := a.routedTo([]string{m.ID})[m.ID]; to != "" {
		t.Fatalf("routed to %q, want nobody in particular", to)
	}
	a.wakeIdle(context.Background())
	if p.count() != 1 || !strings.HasPrefix(p.calls[0], sockNew+"|") || !strings.Contains(p.calls[0], "id "+m.ID) {
		t.Fatalf("posts %v, want the active session woken", p.calls)
	}
	// A hook event makes the old session active again: its chat is its own.
	a.LeaseRevoke("s-new", []string{m.ID}, "inbox_dropped")
	reg("s-old", testSocket, false)
	if to := a.routedTo([]string{m.ID})[m.ID]; to != "s-old" {
		t.Fatalf("after an event: routed to %q", to)
	}
}
