package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDirect is a DirectLauncher: Run gets session started (none when
// empty), runs during (the turn) and returns err.
type fakeDirect struct {
	fakeLauncher
	direct  bool
	session string
	err     error
	during  func()
	runs    []LaunchSpec
	runMu   sync.Mutex
}

func (l *fakeDirect) Direct(string) bool { return l.direct }

func (l *fakeDirect) Run(_ context.Context, spec LaunchSpec, started func(string)) error {
	l.runMu.Lock()
	l.runs = append(l.runs, spec)
	l.runMu.Unlock()
	if l.session != "" {
		started(l.session)
	}
	if l.during != nil {
		l.during()
	}
	return l.err
}

func (l *fakeDirect) allRuns() []LaunchSpec {
	l.runMu.Lock()
	defer l.runMu.Unlock()
	return slices.Clone(l.runs)
}

func TestClaudeArgsAndDeepLink(t *testing.T) {
	if got := strings.Join(ClaudeArgs(LaunchSpec{Prompt: "x y"}), " "); got != "-p --output-format stream-json --verbose --permission-mode bypassPermissions" {
		t.Errorf("new: %q", got)
	}
	if got := strings.Join(ClaudeArgs(LaunchSpec{ResumeID: "abc"}), " "); got != "-p --output-format stream-json --verbose --permission-mode bypassPermissions --resume abc" {
		t.Errorf("resume: %q", got)
	}
	if got := DeepLink(ProviderClaude, "a-1"); got != "claude://resume?session=a-1" {
		t.Errorf("claude link %q", got)
	}
	if got := DeepLink(ProviderCodex, "t-1"); got != "codex://threads/t-1" {
		t.Errorf("codex link %q", got)
	}
}

func TestReadClaudeStream(t *testing.T) {
	var got []string
	started := func(id string) { got = append(got, id) }
	in := `{"type":"system","subtype":"init","session_id":"s-1"}` + "\n" + `not json` + "\n" +
		`{"type":"assistant","session_id":"s-1"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"session_id":"s-1"}` + "\n"
	id, err := ReadClaudeStream(strings.NewReader(in), started)
	if err != nil || id != "s-1" || !slices.Equal(got, []string{"s-1"}) {
		t.Fatalf("ok: %q %v %v", id, err, got)
	}
	got = nil
	in = `{"type":"system","subtype":"init","session_id":"s-2"}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}` + "\n"
	if id, err := ReadClaudeStream(strings.NewReader(in), started); id != "s-2" || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error: %q %v", id, err)
	}
	if id, err := ReadClaudeStream(strings.NewReader("Error: not logged in\n"), started); id != "" || err == nil {
		t.Fatalf("no session: %q %v", id, err)
	}
}

// fakeAppServer answers CodexTurn like codex app-server: it records the
// requests, asks one approval mid-turn and completes the turn with status;
// with early, it completes the turn before it answers turn/start.
func fakeAppServer(t *testing.T, r io.Reader, w io.Writer, status string, early bool, seen *[]string) {
	t.Helper()
	sc := bufio.NewScanner(r)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Errorf("bad json %q", sc.Text())
			return
		}
		method, _ := m["method"].(string)
		params, _ := m["params"].(map[string]any)
		if method == "" { // our answer to the approval request
			res, _ := m["result"].(map[string]any)
			decision, _ := res["decision"].(string)
			*seen = append(*seen, "reply:"+decision)
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "th-x",
				"turn": map[string]any{"id": "other", "status": "completed"}}}) // another turn: ignored
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "th-1",
				"turn": map[string]any{"id": "tu-1", "status": status, "error": map[string]any{"message": "quota"}}}})
			continue
		}
		*seen = append(*seen, method)
		id := m["id"]
		switch method {
		case "initialize":
			_ = enc.Encode(map[string]any{"id": id, "result": map[string]any{"userAgent": "x"}})
		case "thread/start":
			if params["cwd"] != `C:\p` || params["approvalPolicy"] != "never" || params["sandbox"] != "danger-full-access" {
				t.Errorf("thread/start %v", params)
			}
			_ = enc.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "th-1"}}})
		case "thread/resume":
			if params["threadId"] == "busy" {
				_ = enc.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": "already has an active writer"}})
				continue
			}
			if params["threadId"] != "th-1" {
				t.Errorf("resume %v", params)
			}
			_ = enc.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "th-1"}}})
		case "turn/start":
			input, _ := params["input"].([]any)
			var in map[string]any
			if len(input) == 1 {
				in, _ = input[0].(map[string]any)
			}
			sandbox, _ := params["sandboxPolicy"].(map[string]any)
			if params["threadId"] != "th-1" || in["type"] != "text" || in["text"] != "line1\n\"q\"" ||
				params["approvalPolicy"] != "never" || sandbox["type"] != "dangerFullAccess" {
				t.Errorf("turn/start %v", params)
			}
			_ = enc.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "th-1"}})
			if early { // a fast turn: done before its answer
				_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "th-1",
					"turn": map[string]any{"id": "tu-1", "status": status}}})
				_ = enc.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "tu-1", "status": "inProgress"}}})
				continue
			}
			_ = enc.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "tu-1", "status": "inProgress"}}})
			_ = enc.Encode(map[string]any{"id": 99, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
		}
	}
}

func TestCodexTurn(t *testing.T) {
	for _, c := range []struct {
		resume, status string
		early, wantErr bool
	}{{"", "completed", false, false}, {"th-1", "completed", false, false}, {"busy", "completed", false, false}, {"", "failed", false, true},
		{"", "interrupted", false, true}, {"", "completed", true, false}, {"", "interrupted", true, true}} {
		sr, cw := io.Pipe() // client -> server
		cr, sw := io.Pipe() // server -> client
		var seen []string
		done := make(chan struct{})
		go func() {
			defer close(done)
			fakeAppServer(t, sr, sw, c.status, c.early, &seen)
			_ = sw.Close()
		}()
		var started []string
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		id, err := CodexTurn(ctx, cr, cw, LaunchSpec{Provider: ProviderCodex, Folder: `C:\p`, ResumeID: c.resume, Prompt: "line1\n\"q\""},
			"1.0", func(s string) { started = append(started, s) })
		cancel()
		_ = cw.Close()
		<-done
		if id != "th-1" || (err != nil) != c.wantErr || !slices.Equal(started, []string{"th-1"}) {
			t.Fatalf("%+v: id %q err %v started %v", c, id, err, started)
		}
		if c.status == "interrupted" && !errors.Is(err, ErrTurnInterrupted) {
			t.Fatalf("%+v: interrupted: %v", c, err)
		}
		want := []string{"initialize", "initialized", "thread/start", "turn/start", "reply:decline"}
		if c.early {
			want = want[:4]
		}
		switch c.resume {
		case "th-1":
			want[2] = "thread/resume"
		case "busy": // refused: a new thread
			want = slices.Insert(want, 2, "thread/resume")
		}
		if !slices.Equal(seen, want) {
			t.Fatalf("%+v: requests %v", c, seen)
		}
	}
	// An error answer ends it before any turn.
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(sr)
		sc.Scan()
		_, _ = sw.Write([]byte(`{"id":1,"error":{"code":-1,"message":"nope"}}` + "\n"))
		_, _ = io.Copy(io.Discard, sr)
		_ = sw.Close()
	}()
	_, err := CodexTurn(context.Background(), cr, cw, LaunchSpec{Folder: `C:\p`}, "", func(string) { t.Error("started") })
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error answer: %v", err)
	}
	_ = cw.Close()
	<-done
}

// In desktop mode the node claims the messages, runs the first turn itself
// with them as its prompt and reads them as the opened session's only once
// the turn succeeded; it is the area's last session from then on (its
// provider is the next launch's), but never resumed.
func TestLaunchDirect(t *testing.T) {
	l := &fakeDirect{direct: true, session: "sess-1"}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	m := ask(t, a, b, "please look")
	l.during = func() { // the turn runs: not acknowledged yet
		if p, _ := a.Unread("", "", 10); len(p.Messages) != 1 {
			t.Errorf("acknowledged before the turn ended: %+v", p.Messages)
		}
	}
	ctx := context.Background()
	later := time.Now().Add(launchGrace + time.Second)
	a.launchDue(ctx, later)
	a.directWG.Wait()
	runs := l.allRuns()
	if len(runs) != 1 || len(l.all()) != 0 || runs[0].Folder != dir || runs[0].ResumeID != "" ||
		!strings.HasPrefix(runs[0].Prompt, "agent-link: новые сообщения (1).") || !strings.Contains(runs[0].Prompt, "please look") ||
		!strings.Contains(runs[0].Prompt, "id "+m.ID) {
		t.Fatalf("runs %+v launches %+v", runs, l.all())
	}
	waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchConfirmed)
	if p, _ := a.Unread("", "", 10); len(p.Messages) != 0 {
		t.Fatalf("still unread: %+v", p.Messages)
	}
	if len(a.launchHeld()) != 0 {
		t.Fatalf("launch claim kept: %v", a.launchHeld())
	}
	a.sess.mu.Lock()
	ls := a.sess.recent[""]
	a.sess.mu.Unlock()
	if ls.SessionID != "sess-1" || ls.Provider != ProviderClaude || ls.Folder != dir {
		t.Fatalf("last session %+v", ls)
	}
	l.during = nil
	// The next one is a new session again, never a resume (nothing proves
	// the last one is closed).
	ask(t, a, b, "more")
	a.launchDue(ctx, later.Add(launchDebounce+launchGrace+time.Minute))
	a.directWG.Wait()
	if runs := l.allRuns(); len(runs) != 2 || runs[1].ResumeID != "" {
		t.Fatalf("resumed %+v", runs)
	}
}

// While the first turn runs, the launch holds the messages: a session that
// registers meanwhile neither sees nor claims them (no double delivery).
func TestLaunchDirectHoldsClaim(t *testing.T) {
	l := &fakeDirect{direct: true, session: "sess-1"}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	m := ask(t, a, b, "only once")
	l.during = func() {
		if _, err := a.RegisterSession(SessionRequest{SessionID: "s-other", Provider: "claude", Folder: dir}); err != nil {
			t.Error(err)
			return
		}
		if p, _ := a.UnreadFor(dir, "s-other", "", 10); len(p.Messages) != 0 {
			t.Errorf("hooks see the launch's message: %+v", p.Messages)
		}
		if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-other"}); len(got) != 0 {
			t.Errorf("claimed the launch's message: %v", got)
		}
		var um UnreadMessage
		um.Message = m
		if msgs := a.launchClaim("", []UnreadMessage{um}); len(msgs) != 0 {
			t.Errorf("claimed twice: %v", msgs)
		}
	}
	a.launchDue(context.Background(), time.Now().Add(launchGrace+time.Second))
	a.directWG.Wait()
	waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchConfirmed)
	res, _ := a.Ack("", AckRequest{IDs: []string{m.ID}})
	if len(res) != 1 || res[0].WasUnread || res[0].Assigned != "session:sess-1" {
		t.Fatalf("not the opened session's: %+v", res)
	}
}

// A desktop launch that never starts is launch_failed, leaves the message
// unread and not spent, and opens Terminal once instead; launch mode
// terminal, or no desktop app, opens Terminal directly.
func TestLaunchDirectFallback(t *testing.T) {
	l := &fakeDirect{direct: true, err: errors.New("spawn failed")}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	m := ask(t, a, b, "hello")
	ctx := context.Background()
	t0 := time.Now().Add(launchGrace + time.Second)
	a.launchDue(ctx, t0)
	a.directWG.Wait()
	waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchFailed+":start_error")
	if p, _ := a.Unread("", "", 10); len(p.Messages) != 1 {
		t.Fatalf("unread %+v", p.Messages)
	}
	if got := l.all(); len(got) != 1 || got[0].ResumeID != "" || got[0].Folder != dir {
		t.Fatalf("terminal fallback %+v", got)
	}
	a.deliv.mu.Lock()
	spent, fallback := a.deliv.isSpent(m.ID), a.deliv.pending[""]
	clear(a.deliv.pending) // the fallback is not awaited
	a.deliv.mu.Unlock()
	if spent || fallback == nil || fallback.direct {
		t.Fatalf("spent %v pending %+v", spent, fallback)
	}

	a.SetLaunchMode(LaunchTerminal)
	ask(t, a, b, "via terminal")
	t1 := t0.Add(launchDebounce + launchGrace + time.Minute)
	a.launchDue(ctx, t1)
	if len(l.all()) != 2 || len(l.allRuns()) != 1 {
		t.Fatalf("terminal mode: launches %d runs %d", len(l.all()), len(l.allRuns()))
	}

	a.SetLaunchMode(LaunchDesktop)
	l.direct = false // no desktop app
	a.deliv.mu.Lock()
	clear(a.deliv.pending)
	a.deliv.mu.Unlock()
	ask(t, a, b, "no app")
	a.launchDue(ctx, t1.Add(launchDebounce+launchGrace+time.Minute))
	if len(l.all()) != 3 || len(l.allRuns()) != 1 {
		t.Fatalf("no desktop app: launches %d runs %d", len(l.all()), len(l.allRuns()))
	}
}

// A first turn that started but failed or timed out proves nothing: the
// messages stay unread, the claim is dropped (a session's hooks may take
// them), launch_failed:<reason> and needs_human; neither Terminal nor a new
// launch repeats the turn.
func TestLaunchDirectTurnFailed(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
	}{{errors.New("boom"), "turn_error"}, {fmt.Errorf("%w: killed", ErrTurnTimeout), "timeout"}, {fmt.Errorf("%w: x", ErrTurnInterrupted), "interrupted"}} {
		t.Run(c.reason, func(t *testing.T) {
			l := &fakeDirect{direct: true, session: "sess-f", err: c.err}
			dir := t.TempDir()
			a, b := deliveryPair(t, dir, nil, l)
			m := ask(t, a, b, "fail me")
			a.launchDue(context.Background(), time.Now().Add(launchGrace+time.Second))
			a.directWG.Wait()
			waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchFailed+":"+c.reason, AttemptNeedsHuman)
			if p, _ := a.Unread("", "", 10); len(p.Messages) != 1 {
				t.Fatalf("unread %+v", p.Messages)
			}
			if len(a.launchHeld()) != 0 || len(l.all()) != 0 {
				t.Fatalf("claim %v terminal %d", a.launchHeld(), len(l.all()))
			}
			// Not launched for again either: the turn may have acted in part.
			a.launchDue(context.Background(), time.Now().Add(launchDebounce+launchGrace+time.Minute))
			a.directWG.Wait()
			if len(l.allRuns()) != 1 || len(l.all()) != 0 {
				t.Fatalf("relaunched: runs %d terminal %d", len(l.allRuns()), len(l.all()))
			}
			if _, err := a.RegisterSession(SessionRequest{SessionID: "s-h", Provider: "claude", Folder: dir}); err != nil {
				t.Fatal(err)
			}
			// Durable: after a restart it is still not launched for, and
			// needs_human is not reported again.
			restartLaunchState(t, a)
			a.deliv.mu.Lock()
			resent := a.deliv.sent[m.ID+"|"+AttemptNeedsHuman]
			a.deliv.mu.Unlock()
			if !resent {
				t.Fatal("needs_human not kept")
			}
			a.launchDue(context.Background(), time.Now().Add(2*launchDebounce+launchGrace+time.Minute))
			a.directWG.Wait()
			if len(l.allRuns()) != 1 || len(l.all()) != 0 {
				t.Fatalf("relaunched after restart: runs %d terminal %d", len(l.allRuns()), len(l.all()))
			}
			// Only the launch is suppressed: the hooks deliver it (a person
			// decides).
			if _, err := a.RegisterSession(SessionRequest{SessionID: "s-h", Provider: "claude", Folder: dir}); err != nil {
				t.Fatal(err)
			}
			if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-h"}); len(got) != 1 {
				t.Fatalf("hooks cannot take it: %v", got)
			}
			// Pruned once read.
			if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-h"}); err != nil {
				t.Fatal(err)
			}
			a.pruneLaunchState(time.Now())
			restartLaunchState(t, a)
			a.deliv.mu.Lock()
			left := len(a.deliv.spent)
			a.deliv.mu.Unlock()
			if left != 0 {
				t.Fatalf("read message not pruned: %d", left)
			}
		})
	}
}

// restartLaunchState drops a's in-memory launch state and claims and loads
// them again from its data directory, as a restart does.
func restartLaunchState(t *testing.T, a *testNode) {
	t.Helper()
	a.sess.claimMu.Lock()
	clear(a.sess.claims)
	a.sess.claimMu.Unlock()
	a.deliv = newDeliveryState()
	if err := a.loadLaunchState(); err != nil {
		t.Fatal(err)
	}
}

// Spent messages older than spentKeep are pruned even while unread.
func TestLaunchSpentExpires(t *testing.T) {
	a, b := deliveryPair(t, t.TempDir(), nil, &fakeDirect{})
	m := ask(t, a, b, "old")
	now := time.Now()
	a.spend([]string{m.ID}, now.Add(-spentKeep-time.Hour), AttemptNeedsHuman)
	a.spend([]string{"other-unread-gone"}, now)
	a.pruneLaunchState(now)
	restartLaunchState(t, a)
	a.deliv.mu.Lock()
	n := len(a.deliv.spent)
	a.deliv.mu.Unlock()
	if n != 0 {
		t.Fatalf("not pruned: %d", n)
	}
}

// A turn that succeeded is confirmed only once its messages are acknowledged:
// a failed ack is retried; one that keeps failing keeps them claimed by the
// opened session (no other session, hook or launch takes them), durably, and
// is retried later until it succeeds (launch_confirmed).
func TestLaunchDirectAckRetry(t *testing.T) {
	for _, fails := range []int{launchAckTries - 1, launchAckTries} {
		t.Run(fmt.Sprint(fails), func(t *testing.T) {
			l := &fakeDirect{direct: true, session: "sess-a"}
			dir := t.TempDir()
			a, b := deliveryPair(t, dir, nil, l)
			var calls atomic.Int32
			var broken atomic.Bool
			broken.Store(true)
			a.launchAck = func(req AckRequest) error {
				if int(calls.Add(1)) <= fails || (fails >= launchAckTries && broken.Load()) {
					return errors.New("store busy")
				}
				_, err := a.Ack("", req)
				return err
			}
			m := ask(t, a, b, "ack me")
			a.launchDue(context.Background(), time.Now().Add(launchGrace+time.Second))
			a.directWG.Wait()
			a.deliv.mu.Lock()
			spent := a.deliv.isSpent(m.ID)
			a.deliv.mu.Unlock()
			unread, _ := a.Unread("", "", 10)
			if fails < launchAckTries {
				waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchConfirmed)
				if !spent || len(unread.Messages) != 0 {
					t.Fatalf("spent %v unread %+v", spent, unread.Messages)
				}
				return
			}
			if len(unread.Messages) != 1 || !a.launchHeld()[m.ID] || len(l.all()) != 0 {
				t.Fatalf("unread %d held %v terminal %d", len(unread.Messages), a.launchHeld(), len(l.all()))
			}
			// Held by the opened session, across a restart: no other session
			// sees or claims it, and no launch opens for it.
			restartLaunchState(t, a)
			if !a.launchHeld()[m.ID] {
				t.Fatal("ack job not restored")
			}
			if _, err := a.RegisterSession(SessionRequest{SessionID: "s-other", Provider: "claude", Folder: dir}); err != nil {
				t.Fatal(err)
			}
			if p, _ := a.UnreadFor(dir, "s-other", "", 10); len(p.Messages) != 0 {
				t.Fatalf("another session sees it: %+v", p.Messages)
			}
			if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-other"}); len(got) != 0 {
				t.Fatalf("another session claimed it: %v", got)
			}
			a.retryLaunchAcks(context.Background(), time.Now()) // due (restored), still failing
			if len(a.launchHeld()) != 1 {
				t.Fatalf("dropped while failing: %v", a.launchHeld())
			}
			broken.Store(false)
			a.retryLaunchAcks(context.Background(), time.Now().Add(launchAckRetry+time.Second))
			waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchConfirmed)
			res, _ := a.Ack("", AckRequest{IDs: []string{m.ID}})
			a.deliv.mu.Lock()
			jobs := len(a.deliv.acks)
			a.deliv.mu.Unlock()
			if len(res) != 1 || res[0].WasUnread || res[0].Assigned != "session:sess-a" || jobs != 0 || len(a.launchHeld()) != 0 {
				t.Fatalf("ack %+v jobs %d held %v", res, jobs, a.launchHeld())
			}
		})
	}
}

// A folder occupied by a session that is not registered opens nothing
// (needs_human), neither directly nor as the Terminal fallback of a desktop
// launch that failed to start; a Terminal start error does not spend the
// message.
func TestLaunchOccupied(t *testing.T) {
	var occupied atomic.Bool
	occupied.Store(true)
	l := &fakeDirect{direct: true, err: errors.New("spawn failed")}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	a.occupied = func(string, time.Time) bool { return occupied.Load() }
	m := ask(t, a, b, "someone is here")
	ctx := context.Background()
	t0 := time.Now().Add(launchGrace + time.Second)
	a.launchDue(ctx, t0)
	a.directWG.Wait()
	if len(l.allRuns()) != 0 || len(l.all()) != 0 {
		t.Fatalf("launched into an occupied folder: runs %d terminal %d", len(l.allRuns()), len(l.all()))
	}
	waitAttempts(t, b, m, AttemptNeedsHuman)

	// Free: the desktop launch fails before its turn starts; the folder is
	// occupied by then, so no Terminal either.
	occupied.Store(false)
	l.during = func() { occupied.Store(true) }
	a.launchDue(ctx, t0.Add(time.Second))
	a.directWG.Wait()
	if len(l.allRuns()) != 1 || len(l.all()) != 0 {
		t.Fatalf("fallback into an occupied folder: runs %d terminal %d", len(l.allRuns()), len(l.all()))
	}

	// Terminal only, start error: reported, not spent.
	occupied.Store(false)
	a.SetLaunchMode(LaunchTerminal)
	l.mu.Lock()
	l.err = ErrNoTerminal
	l.mu.Unlock()
	a.launchDue(ctx, t0.Add(launchDebounce+launchGrace+time.Minute))
	a.deliv.mu.Lock()
	spent := a.deliv.isSpent(m.ID)
	a.deliv.mu.Unlock()
	if len(l.all()) != 1 || spent {
		t.Fatalf("terminal %d spent %v", len(l.all()), spent)
	}
}

// The occupancy check reads Claude Code's transcripts and Codex's rollouts:
// recent activity in the folder (a Codex one also in a subfolder) occupies it.
func TestAgentOccupied(t *testing.T) {
	if got := claudeProjectDir(`E:\DEV\agent-link\.data_x y`); got != "E--DEV-agent-link--data-x-y" {
		t.Fatalf("claudeProjectDir %q", got)
	}
	claude, codex, dir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now()
	if agentOccupied(claude, codex, dir, now) {
		t.Fatal("empty homes occupy")
	}
	proj := filepath.Join(claude, "projects", claudeProjectDir(dir))
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(proj, "11111111-2222-3333-4444-555555555555.jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !agentOccupied(claude, codex, dir, now) {
		t.Fatal("a fresh Claude transcript does not occupy")
	}
	if agentOccupied(claude, codex, dir, now.Add(occupiedWithin+time.Minute)) {
		t.Fatal("an old Claude transcript occupies")
	}
	day := now.Local()
	sub := filepath.Join(codex, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"cwd": filepath.Join(dir, "sub")}})
	if err := os.WriteFile(filepath.Join(sub, "rollout-x.jsonl"), append(meta, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if !agentOccupied("", codex, dir, now) {
		t.Fatal("a fresh Codex rollout in the folder does not occupy")
	}
	if agentOccupied("", codex, t.TempDir(), now) {
		t.Fatal("a Codex rollout of another folder occupies")
	}
}
