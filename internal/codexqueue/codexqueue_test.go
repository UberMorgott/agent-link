package codexqueue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake codex: with FAKE_CODEX_VERSION set it
// answers `--version` with that version, and `queue ...` writes its arguments
// and CODEX_HOME to FAKE_CODEX_OUT (FAKE_CODEX_FAIL: fails instead).
func TestMain(m *testing.M) {
	if v := os.Getenv("FAKE_CODEX_VERSION"); v != "" && len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(fakeCodex(v, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeCodex(version string, args []string) int {
	switch args[0] {
	case "--version":
		fmt.Println("codex-cli " + version)
		return 0
	case "queue":
		if os.Getenv("FAKE_CODEX_FAIL") != "" {
			fmt.Fprintln(os.Stderr, "thread not found")
			return 1
		}
		line := strings.Join(args, "|") + "|home=" + os.Getenv("CODEX_HOME")
		if err := os.WriteFile(os.Getenv("FAKE_CODEX_OUT"), []byte(line), 0o600); err != nil {
			return 2
		}
		return 0
	}
	return 2
}

// fake returns a Queue whose only real candidate is the fake codex of version.
func fake(t *testing.T, version string) (*Queue, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "queued.txt")
	t.Setenv("FAKE_CODEX_VERSION", version)
	t.Setenv("FAKE_CODEX_OUT", out)
	q := NewWith(func() []string { return []string{filepath.Join(t.TempDir(), "missing.exe"), exe} })
	q.Check(context.Background(), true)
	return q, out
}

func TestQueueWake(t *testing.T) {
	q, out := fake(t, "0.155.1")
	if !q.Ready() {
		t.Fatal("codex 0.155.1 not ready")
	}
	if _, v := q.Binary(); v != "0.155.1" {
		t.Fatalf("version %q", v)
	}
	home := t.TempDir()
	if err := q.Wake(context.Background(), home, "thread-1", "agent-link: 2 новых сообщения — прочитай их"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out) //nolint:gosec // G304: the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	want := "queue|--thread|thread-1|--message|agent-link: 2 новых сообщения — прочитай их|home=" + home
	if string(got) != want {
		t.Fatalf("codex ran with %q, want %q", got, want)
	}
	if err := q.Wake(context.Background(), home, "-x", "hi"); err == nil {
		t.Fatal("a thread id like a flag was queued")
	}

	// A failed wake: not Ready until downFor passes.
	t.Setenv("FAKE_CODEX_FAIL", "1")
	if err := q.Wake(context.Background(), "", "thread-1", "hi"); err == nil || !strings.Contains(err.Error(), "thread not found") {
		t.Fatalf("failed wake: %v", err)
	}
	if q.Ready() {
		t.Fatal("ready right after a failed wake")
	}
	later := time.Now().Add(downFor + time.Second)
	q.now = func() time.Time { return later }
	if !q.Ready() {
		t.Fatal("not ready after downFor")
	}
}

func TestQueueNeedsNewCodex(t *testing.T) {
	q, _ := fake(t, "0.148.9")
	if q.Ready() {
		t.Fatal("codex 0.148.9 is ready")
	}
	if err := q.Wake(context.Background(), "", "thread-1", "hi"); err == nil {
		t.Fatal("woke without a usable codex")
	}
	none := NewWith(func() []string { return nil })
	none.Check(context.Background(), false)
	if none.Ready() {
		t.Fatal("ready with no codex")
	}
}

func TestAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"0.149.0": true, "0.155.1": true, "v0.150.0-alpha.1": true, "1.0": true, "0.149": true,
		"0.148.9": false, "0.9.0": false, "": false, "x.y": false, "0": false,
	} {
		if got := AtLeast(v, MinVersion); got != want {
			t.Errorf("AtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}
