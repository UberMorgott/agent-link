// Package fsutil holds file system helpers shared by the stores.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"time"
)

// ReplaceFile renames src over dst (the last step of an atomic write). On
// Windows the replace fails with "Access is denied" while another handle has
// dst open without delete sharing: an antivirus or the indexer scanning the
// file just written, or a reader. That clears within moments, so the rename is
// retried for up to about 0.6s before the error counts; a failed write would
// otherwise lose the update (a message's session, a settings change).
func ReplaceFile(src, dst string) error {
	err := os.Rename(src, dst)
	for delay := 5 * time.Millisecond; err != nil && transient(err) && delay <= 320*time.Millisecond; delay *= 2 {
		time.Sleep(delay)
		err = os.Rename(src, dst)
	}
	return err
}

// transient reports a rename error that a moment later may not happen again.
func transient(err error) bool {
	return runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission)
}
