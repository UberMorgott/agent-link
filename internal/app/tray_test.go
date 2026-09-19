package app

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
)

func TestTrayTooltip(t *testing.T) {
	online := Status{Configured: true, Connected: true, Online: 7, Total: 12}
	cases := []struct {
		name string
		s    Status
		u    UpdateStatus
		want string
	}{
		{"members", online, UpdateStatus{}, "agentlink — На связи 7 из 12"},
		{"update", online, UpdateStatus{Available: true, Latest: "0.6.0"}, "agentlink — На связи 7 из 12 · доступна v0.6.0"},
		{"checked, none newer", online, UpdateStatus{Latest: "0.5.1"}, "agentlink — На связи 7 из 12"},
		{"rate limited", online, UpdateStatus{Available: true, Latest: "0.6.0", Failed: true, RetryAt: "15:04"}, "agentlink — На связи 7 из 12 · обновление после 15:04"},
		{"not set up", Status{}, UpdateStatus{}, "agentlink — Не настроено — откройте настройки"},
	}
	for _, c := range cases {
		if got := TrayTooltip(c.s, c.u); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	long := TrayTooltip(Status{Configured: true, Connected: true, Total: 1, Peer: strings.Repeat("я", 200)}, UpdateStatus{})
	if n := len(utf16.Encode([]rune(long))); n > maxTooltip || !strings.HasSuffix(long, "…") {
		t.Fatalf("long tooltip: %d UTF-16 units, %q", n, long)
	}
}

func TestAutostartCommand(t *testing.T) {
	exe := filepath.Join("C:", "Program Files", "agentlink", "agentlink.exe")
	for _, p := range []string{exe, selfupdate.OldPath(exe), filepath.Join(filepath.Dir(exe), ".agentlink.exe.new")} {
		if got, want := runCommand(p), `"`+exe+`"`; got != want {
			t.Errorf("runCommand(%q) = %s, want %s", p, got, want)
		}
	}
	for _, p := range []string{filepath.Join("C:", "x", ".old"), filepath.Join("C:", "x", "tool.old")} {
		if got := stableExe(p); got != p {
			t.Errorf("stableExe(%q) = %q, want it unchanged", p, got)
		}
	}
}

func TestApprovedDisabled(t *testing.T) {
	for _, c := range []struct {
		v    []byte
		want bool
	}{
		{nil, false},
		{[]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, false},
		{[]byte{6, 0, 0, 0}, false},
		{[]byte{3, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}, true},
		{[]byte{1}, true},
	} {
		if got := approvedDisabled(c.v); got != c.want {
			t.Errorf("approvedDisabled(% x) = %v, want %v", c.v, got, c.want)
		}
	}
}

// fakeAutostart is the Run entry as seen through SetAutostart/AutostartState.
type fakeAutostart struct {
	on    bool
	calls []bool
}

func (f *fakeAutostart) install(a *App) {
	a.SetAutostart = func(on bool) error { f.calls = append(f.calls, on); f.on = on; return nil }
	a.AutostartState = func() (bool, error) { return f.on, nil }
}

func TestSetAutostartNowSaves(t *testing.T) {
	f := &fakeAutostart{}
	h := newHarness(t, f.install)
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if err := h.app.SetAutostartNow(false); err != nil {
		t.Fatal(err)
	}
	if on, avail := h.app.Autostart(); on || !avail || f.on {
		t.Fatalf("Autostart() = %v, %v; entry %v", on, avail, f.on)
	}
	if s, _, err := settings.Load(h.path); err != nil || s.Autostart {
		t.Fatalf("saved autostart %v, err %v", s.Autostart, err)
	}
	h.app.SetAutostart = nil
	if err := h.app.SetAutostartNow(true); !errors.Is(err, ErrNoAutostart) {
		t.Fatalf("without autostart: %v", err)
	}
	if _, avail := h.app.Autostart(); avail {
		t.Fatal("available without SetAutostart")
	}
}

// The page shows and changes the Run entry as Windows has it, even when the
// saved setting went stale (the tray or Task Manager changed it).
func TestSettingsPageFollowsRealAutostart(t *testing.T) {
	f := &fakeAutostart{}
	var told []bool
	h := newHarness(t, f.install, func(a *App) { a.AutostartChanged = func(on bool) { told = append(told, on) } })
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	f.on = false // switched off in Task Manager; the config still says on
	if _, body := h.do(t, http.MethodGet, "/ui/api/settings", "", h.tokenHdr()); !strings.Contains(body, `"autostart":false`) {
		t.Fatalf("GET settings shows the stale setting: %s", body)
	}
	// Saving with autostart on turns the entry on again.
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("resave: %d %s", code, body)
	}
	if !f.on || len(f.calls) != 2 || len(told) != 2 || !told[1] {
		t.Fatalf("entry %v, calls %v, told %v", f.on, f.calls, told)
	}
}
