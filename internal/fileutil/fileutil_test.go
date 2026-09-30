package fileutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The lock is exclusive and owned: a second taker waits and then fails, a
// holder that was slow keeps it (no staleness), and once released the next
// taker gets it at once.
func TestLockIsExclusiveAndOwned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.lock")
	unlock, err := Lock(t.Context(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(t.Context(), path, 50*time.Millisecond); err == nil {
		t.Fatal("a second holder got a held lock")
	}
	if _, ok := TryLock(path); ok {
		t.Fatal("TryLock got a held lock")
	}
	// Old as it may be, the file does not make the lock stale.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := TryLock(path); ok {
		t.Fatal("an old lock file was taken as stale")
	}
	unlock()
	again, ok := TryLock(path)
	if !ok {
		t.Fatal("a released lock was not free")
	}
	again()
}

func TestWriteAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b.json")
	for _, data := range []string{"one", "two"} {
		if err := WriteAtomic(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Clean(path)); err != nil || string(got) != data {
			t.Fatalf("read %q %v, want %q", got, err, data)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temporary files: %v", entries)
	}
}

// A reader holding the state file open for a moment (the waiter polling it)
// must not make a hook's save fail: on Windows the rename is then refused.
func TestWriteAtomicWaitsForReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteAtomic(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = f.Close()
		close(closed)
	}()
	err = WriteAtomic(path, []byte("new"))
	<-closed
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Clean(path)); string(got) != "new" {
		t.Fatalf("state = %q", got)
	}
}
