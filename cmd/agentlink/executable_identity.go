package main

import "os"

// executableReplaced reports when a newer executable occupies the path this
// process started from. A missing path during the update's rename is not a
// replacement: only a different file at that path retires this process.
func executableReplaced() func() bool {
	path, err := os.Executable()
	if err != nil {
		return func() bool { return false }
	}
	return fileReplaced(path)
}

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
