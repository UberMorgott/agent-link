package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A target held open without delete sharing (as a scanner holds a file just
// written) blocks the replace for a moment: ReplaceFile waits it out.
func TestReplaceFileWaitsForOpenTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err == nil {
		t.Fatal("rename over an open target succeeded: the test does not hold it")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.CloseHandle(h)
	}()
	if err := ReplaceFile(src, dst); err != nil {
		t.Fatalf("replace after the target closed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Clean(dst)); err != nil || string(b) != src {
		t.Fatalf("dst after replace: %q %v", b, err)
	}
}
