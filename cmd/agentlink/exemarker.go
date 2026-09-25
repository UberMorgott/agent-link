package main

import "path/filepath"

// exeMarkerName is the file, next to the default settings file, that holds the
// absolute path of the running desktop app's executable. The plugin launchers
// (plugins/agent-link/bin) read it to find agentlink without it on PATH.
const exeMarkerName = "executable.path"

// writeExeMarker atomically records exe in dir's marker file: the path alone,
// no newline, so a batch file's `set /p` reads it verbatim.
func writeExeMarker(dir, exe string) error {
	return writeFileAtomic(filepath.Join(dir, exeMarkerName), []byte(exe))
}
