package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The executable an update parked stays while an old process (an MCP server,
// a waiter) still runs it, and goes at a later try once it exited; files that
// are not the updater's own are never touched.
func TestCleanupLoopWaitsForInUseOldExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open file blocks its removal only on Windows")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentlink.exe")
	old := filepath.Join(dir, ".agentlink.exe.old.1")
	mine := filepath.Join(dir, "notes.txt")
	for _, p := range []string{exe, old, mine} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(filepath.Clean(old)) // an old process still runs it
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		cleanupLoop(ctx, exe, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, 3, 20*time.Millisecond)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(old); err != nil {
		t.Fatal("in-use old executable removed")
	}
	_ = f.Close() // the old process exits
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never finished")
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("old executable left")
	}
	for _, p := range []string{exe, mine} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
}
