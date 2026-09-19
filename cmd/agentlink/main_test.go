package main

import (
	"bytes"
	"strings"
	"testing"
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
	if code := run([]string{"members"}, &out, &errw); code != 1 {
		t.Fatalf("members without --config: code %d", code)
	}
}
