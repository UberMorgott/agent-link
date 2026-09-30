package main

import "os"

// fileReplaced reports when a newer file occupies path, the one it saw at the
// call (freshExecutable). A missing path during the update's rename is not a
// replacement: only a different file at that path is.
func fileReplaced(path string) func() bool {
	started, err := os.Stat(path)
	if err != nil {
		return func() bool { return false }
	}
	// On Windows, os.SameFile loads a FileInfo's file ID lazily. Resolve the
	// startup ID now, before the updater renames the running executable.
	_ = os.SameFile(started, started)
	return func() bool {
		current, err := os.Stat(path)
		return err == nil && !os.SameFile(started, current)
	}
}
