// Package fileutil holds the file primitives the CLI, its hooks and the hook
// installer share: an atomic file write that survives a reader holding the
// file open on Windows, and an exclusive lock between processes.
package fileutil

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// WriteAtomic replaces path with data through a temporary file in its folder
// (created when missing), so a reader never sees a half-written file.
func WriteAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	// On Windows the rename fails while another process (a session's waiter
	// polling its state, an agent reading its settings) has the file open for
	// a moment: retry.
	for try := 0; ; try++ {
		err = os.Rename(tmp.Name(), path)
		if err == nil || try == 50 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// lockRetry is how often Lock tries again while another process holds the lock.
const lockRetry = 20 * time.Millisecond

// Lock takes the exclusive lock of the file at path (created when missing),
// waiting up to wait (or until ctx ends) for another holder. The operating system owns the lock:
// it is released by unlock, or when the holding process dies, and a slow
// holder cannot lose it to a staleness guess. The file itself stays.
func Lock(ctx context.Context, path string, wait time.Duration) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := flock.New(path)
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ok, err := l.TryLockContext(ctx, lockRetry)
	if !ok {
		if err == nil {
			err = context.DeadlineExceeded
		}
		return nil, err
	}
	return func() { _ = l.Unlock() }, nil
}

// TryLock takes the exclusive lock of the file at path (Lock) only if nobody
// holds it; ok is false when another process does.
func TryLock(path string) (unlock func(), ok bool) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return nil, false
	}
	l := flock.New(path)
	if got, err := l.TryLock(); err != nil || !got {
		return nil, false
	}
	return func() { _ = l.Unlock() }, true
}
