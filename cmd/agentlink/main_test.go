package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/app"
)

func TestAppMode(t *testing.T) {
	for _, c := range []struct {
		args []string
		app  bool
	}{
		{nil, true},
		{[]string{"-config", "x.json"}, true},
		{[]string{"-no-tray", "-api", "127.0.0.1:1"}, true},
		{[]string{"--version"}, true},
		{[]string{"-restarted"}, true},
		{[]string{"version"}, false},
		{[]string{"update", "--check"}, false},
		{[]string{"members", "--config", "x.json"}, false},
		{[]string{"bogus"}, false},
	} {
		if got := appMode(c.args); got != c.app {
			t.Errorf("appMode(%q) = %v, want %v", c.args, got, c.app)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	var out, errw bytes.Buffer
	if code := run([]string{"version"}, &out, &errw); code != 0 || strings.TrimSpace(out.String()) == "" {
		t.Fatalf("version: code %d, out %q", code, out.String())
	}
	errw.Reset()
	if code := run([]string{"bogus"}, &out, &errw); code != 1 || !strings.Contains(errw.String(), "usage:") {
		t.Fatalf("bogus: code %d, stderr %q", code, errw.String())
	}
	errw.Reset()
	if code := run([]string{"serve"}, &out, &errw); code != 1 || !strings.Contains(errw.String(), "--config is required") {
		t.Fatalf("serve without --config: code %d, stderr %q", code, errw.String())
	}
	t.Setenv(envAPI, "127.0.0.1:1") // nothing listens: the command fails to connect
	if code := run([]string{"members"}, &out, &errw); code != 1 {
		t.Fatalf("members against a dead api: code %d", code)
	}
}

func TestClickDebounce(t *testing.T) {
	d := &debounce{gap: clickGap}
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		after time.Duration
		want  bool
	}{
		{0, true},                        // first click opens the page
		{200 * time.Millisecond, false},  // second WM_LBUTTONUP of a double click
		{900 * time.Millisecond, false},  // still within the gap of the first
		{1200 * time.Millisecond, true},  // a new click
		{1300 * time.Millisecond, false}, // measured from the last one passed
		{2300 * time.Millisecond, true},
	} {
		if got := d.allow(t0.Add(c.after)); got != c.want {
			t.Errorf("click at +%v: allow = %v, want %v", c.after, got, c.want)
		}
	}
}

func TestDashboardURLUsesLauncher(t *testing.T) {
	a, err := app.New(filepath.Join(t.TempDir(), "config.json"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetAPIAddr("127.0.0.1:7631"); err != nil {
		t.Fatal(err)
	}
	if got, want := dashboardURL(a), "http://127.0.0.1:7631/ui/open"; got != want {
		t.Fatalf("dashboardURL() = %q, want %q", got, want)
	}
}

func TestStartupURLShowsSettingsUntilConfigured(t *testing.T) {
	a, err := app.New(filepath.Join(t.TempDir(), "config.json"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetAPIAddr("127.0.0.1:7631"); err != nil {
		t.Fatal(err)
	}
	if got, want := startupURL(a, false), "http://127.0.0.1:7631/ui/settings"; got != want {
		t.Fatalf("unconfigured startupURL() = %q, want %q", got, want)
	}
	if got, want := startupURL(a, true), "http://127.0.0.1:7631/ui/open"; got != want {
		t.Fatalf("configured startupURL() = %q, want %q", got, want)
	}
}
