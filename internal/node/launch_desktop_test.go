package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDirect is a DirectLauncher: Run gets session started (none when
// empty) and returns err.
type fakeDirect struct {
	fakeLauncher
	direct  bool
	session string
	err     error
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
	return l.err
}

func (l *fakeDirect) allRuns() []LaunchSpec {
	l.runMu.Lock()
	defer l.runMu.Unlock()
	return slices.Clone(l.runs)
}

func TestClaudeArgsAndDeepLink(t *testing.T) {
	if got := strings.Join(ClaudeArgs(LaunchSpec{Prompt: "x y"}), " "); got != "-p --output-format stream-json --verbose" {
		t.Errorf("new: %q", got)
	}
	if got := strings.Join(ClaudeArgs(LaunchSpec{ResumeID: "abc"}), " "); got != "-p --output-format stream-json --verbose --resume abc" {
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
// requests, asks one approval mid-turn and completes the turn with status.
func fakeAppServer(t *testing.T, r io.Reader, w io.Writer, status string, seen *[]string) {
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
			if params["cwd"] != `C:\p` {
				t.Errorf("cwd %v", params["cwd"])
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
			if params["threadId"] != "th-1" || in["type"] != "text" || in["text"] != "line1\n\"q\"" {
				t.Errorf("turn/start %v", params)
			}
			_ = enc.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "th-1"}})
			_ = enc.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "tu-1", "status": "inProgress"}}})
			_ = enc.Encode(map[string]any{"id": 99, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
		}
	}
}

func TestCodexTurn(t *testing.T) {
	for _, c := range []struct {
		resume, status string
		wantErr        bool
	}{{"", "completed", false}, {"th-1", "completed", false}, {"busy", "completed", false}, {"", "failed", true}} {
		sr, cw := io.Pipe() // client -> server
		cr, sw := io.Pipe() // server -> client
		var seen []string
		done := make(chan struct{})
		go func() {
			defer close(done)
			fakeAppServer(t, sr, sw, c.status, &seen)
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
		want := []string{"initialize", "initialized", "thread/start", "turn/start", "reply:decline"}
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

// In desktop mode the node runs the first turn itself with the messages as
// its prompt; they are read as the opened session's once it started, and it
// is the area's last session from then on.
func TestLaunchDirect(t *testing.T) {
	l := &fakeDirect{direct: true, session: "sess-1"}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, l)
	m := ask(t, a, b, "please look")
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
	a.sess.mu.Lock()
	ls := a.sess.recent[""]
	a.sess.mu.Unlock()
	if ls.SessionID != "sess-1" || ls.Provider != ProviderClaude || ls.Folder != dir {
		t.Fatalf("last session %+v", ls)
	}
	// The next one resumes it.
	ask(t, a, b, "more")
	a.launchDue(ctx, later.Add(launchDebounce+launchGrace+time.Minute))
	a.directWG.Wait()
	if runs := l.allRuns(); len(runs) != 2 || runs[1].ResumeID != "sess-1" {
		t.Fatalf("resume %+v", runs)
	}
}

// A desktop launch that never starts is launch_failed and leaves the message
// unread; launch mode terminal, or no desktop app, opens Terminal instead.
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

	a.SetLaunchMode(LaunchTerminal)
	ask(t, a, b, "via terminal")
	t1 := t0.Add(launchDebounce + launchGrace + time.Minute)
	a.launchDue(ctx, t1)
	if len(l.all()) != 1 || len(l.allRuns()) != 1 {
		t.Fatalf("terminal mode: launches %d runs %d", len(l.all()), len(l.allRuns()))
	}

	a.SetLaunchMode(LaunchDesktop)
	l.direct = false  // no desktop app
	a.deliv.mu.Lock() // the Terminal launch above is not awaited
	clear(a.deliv.pending)
	a.deliv.mu.Unlock()
	ask(t, a, b, "no app")
	a.launchDue(ctx, t1.Add(launchDebounce+launchGrace+time.Minute))
	if len(l.all()) != 2 || len(l.allRuns()) != 1 {
		t.Fatalf("no desktop app: launches %d runs %d", len(l.all()), len(l.allRuns()))
	}
}
