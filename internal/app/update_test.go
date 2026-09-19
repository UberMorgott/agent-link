package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/settings"
)

// errBox is an error the test sets while the app reads it.
type errBox struct {
	mu  sync.Mutex
	err error
}

func (b *errBox) set(err error) { b.mu.Lock(); b.err = err; b.mu.Unlock() }
func (b *errBox) get() error    { b.mu.Lock(); defer b.mu.Unlock(); return b.err }

type fakeRelease struct {
	version string
	err     errBox
	applied atomic.Int32
}

func (r *fakeRelease) Version() string { return r.version }
func (r *fakeRelease) Apply(context.Context, string) ([]string, error) {
	r.applied.Add(1)
	return nil, r.err.get()
}

// updHarness is an app of version 0.4.0 whose GitHub has rel (nil: none)
// and which counts relaunches and quits.
type updHarness struct {
	*harness
	rel       *fakeRelease
	newer     bool
	checkErr  errBox
	relaunch  atomic.Int32
	quits     atomic.Int32
	relaunchE errBox
}

func newUpdHarness(t *testing.T, rel *fakeRelease, newer bool) *updHarness {
	t.Helper()
	u := &updHarness{rel: rel, newer: newer}
	exe := filepath.Join(t.TempDir(), "agentlink.exe")
	if err := os.WriteFile(exe, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	u.harness = newHarness(t, func(a *App) {
		a.Version = "0.4.0"
		a.SetExecutable(exe)
		a.Latest = func(context.Context, string) (Release, bool, error) {
			if err := u.checkErr.get(); err != nil {
				return nil, false, err
			}
			if u.rel == nil {
				return nil, false, nil
			}
			return u.rel, u.newer, nil
		}
		a.Relaunch = func() error { u.relaunch.Add(1); return u.relaunchE.get() }
		a.QuitFunc = func() { u.quits.Add(1) }
	})
	return u
}

func (u *updHarness) post(t *testing.T, path, body string) UpdateStatus {
	t.Helper()
	code, out := u.do(t, http.MethodPost, "/ui/api/"+path, body, u.tokenHdr())
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, code, out)
	}
	var st UpdateStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUpdateCheckStates(t *testing.T) {
	for _, c := range []struct {
		name      string
		rel       *fakeRelease
		newer     bool
		err       error
		available bool
		text      string
	}{
		{"newer", &fakeRelease{version: "0.5.0"}, true, nil, true, msg("update.available", map[string]string{"version": "0.5.0"})},
		{"equal", &fakeRelease{version: "0.4.0"}, false, nil, false, msg("update.latest", nil)},
		{"none", nil, false, nil, false, msg("update.none", nil)},
		{"error", nil, false, errors.New("offline"), false, msg("update.error.check", nil)},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUpdHarness(t, c.rel, c.newer)
			u.checkErr.set(c.err)
			st := u.post(t, "update/check", "")
			if st.Available != c.available || st.Text != c.text || st.Current != "0.4.0" || !st.Enabled || st.Failed != (c.err != nil) {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

func TestUpdateInstallRestartsWithoutKill(t *testing.T) {
	u := newUpdHarness(t, &fakeRelease{version: "0.5.0"}, true)
	st := u.post(t, "update/apply", "") // checks first
	if !st.Restarting || u.rel.applied.Load() != 1 {
		t.Fatalf("status = %+v, applied %d", st, u.rel.applied.Load())
	}
	waitFor(t, "relaunch and quit", func() bool { return u.relaunch.Load() == 1 && u.quits.Load() == 1 })
	// A second install while restarting does nothing.
	if st := u.post(t, "update/apply", ""); !st.Restarting || u.rel.applied.Load() != 1 {
		t.Fatalf("second install: %+v", st)
	}
}

func TestUpdateInstallFailures(t *testing.T) {
	u := newUpdHarness(t, &fakeRelease{version: "0.5.0"}, true)
	u.rel.err.set(errors.New("checksum mismatch"))
	st := u.post(t, "update/apply", "")
	if st.Restarting || !st.Failed || st.Text != msg("update.error.apply", nil) || u.relaunch.Load() != 0 {
		t.Fatalf("failed apply: %+v", st)
	}
	u.rel.err.set(nil)
	u.relaunchE.set(errors.New("no exe"))
	u.post(t, "update/apply", "")
	waitFor(t, "relaunch error", func() bool { return u.app.UpdateStatus().Failed })
	if st := u.app.UpdateStatus(); st.Text != msg("update.error.restart", nil) || u.quits.Load() != 0 {
		t.Fatalf("failed relaunch: %+v, quits %d", st, u.quits.Load())
	}
}

// When `agentlink update` already replaced the executable, the app only restarts.
func TestUpdateInstallAfterCLIUpdate(t *testing.T) {
	u := newUpdHarness(t, &fakeRelease{version: "0.5.0"}, true)
	if err := os.WriteFile(u.app.Exe, []byte("newer build"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := u.post(t, "update/apply", ""); !st.Restarting || u.rel.applied.Load() != 0 {
		t.Fatalf("status = %+v, applied %d", st, u.rel.applied.Load())
	}
}

func TestUpdateDisabledForDevBuild(t *testing.T) {
	u := newUpdHarness(t, &fakeRelease{version: "0.5.0"}, true)
	u.app.Version = "dev"
	st := u.post(t, "update/apply", "")
	if st.Enabled || st.Available || st.Text != msg("update.disabled", nil) || u.rel.applied.Load() != 0 {
		t.Fatalf("dev build: %+v", st)
	}
	done := make(chan struct{})
	go func() { u.app.RunUpdates(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunUpdates kept running for a dev build")
	}
}

func TestAutoUpdateSwitch(t *testing.T) {
	u := newUpdHarness(t, nil, false)
	code, _ := u.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), u.tokenHdr())
	if code != http.StatusOK {
		t.Fatal("save failed")
	}
	if !u.app.UpdateStatus().Auto {
		t.Fatal("auto-update is off by default")
	}
	if st := u.post(t, "update/auto", `{"auto":false}`); st.Auto {
		t.Fatalf("switch off: %+v", st)
	}
	s, _, err := settings.Load(u.path)
	if err != nil || s.AutoUpdateOn() {
		t.Fatalf("saved auto_update = %v, %v", s.AutoUpdate, err)
	}
	// Saving the form (which carries no auto_update) keeps the switch.
	u.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), u.tokenHdr())
	if u.app.UpdateStatus().Auto {
		t.Fatal("a settings save turned auto-update back on")
	}
}

func TestRunUpdatesInstallsWhenAuto(t *testing.T) {
	u := newUpdHarness(t, &fakeRelease{version: "0.5.0"}, true)
	u.app.UpdateFirst, u.app.UpdateEvery = time.Millisecond, 10*time.Millisecond
	if err := u.app.SetAutoUpdate(false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.app.RunUpdates(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if u.rel.applied.Load() != 0 {
		t.Fatal("installed with auto-update off")
	}
	if err := u.app.SetAutoUpdate(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "auto install", func() bool { return u.quits.Load() == 1 })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunUpdates did not stop after installing")
	}
	cancel()
	if !strings.Contains(u.app.UpdateStatus().Text, "0.5.0") {
		t.Fatalf("text = %q", u.app.UpdateStatus().Text)
	}
}
