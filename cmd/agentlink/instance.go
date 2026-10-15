package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
)

// One desktop app per settings file. A second start of agentlink without a
// command (an agent trying the CLI path its hints name without a command, a
// second double-click) must not become a second tray app: it would only fail
// to take the API address and, without a console, show a modal error for each
// attempt (issue #32). The running app marks itself with a named kernel object
// derived from its settings file; a later start that finds the marker exits
// at once, before it detaches from its console or reads anything. The marker
// does not depend on reaching the API address, which a sandboxed agent shell
// may not be allowed to do.

// instanceRunning reports whether a process holds the marker name. The
// default finds none, where the system has no named kernel objects (the API
// address probe, runningApp, tells there); instance_windows.go sets it.
var instanceRunning = func(string) bool { return false }

// holdInstance creates the marker name and keeps it until release. The
// default holds nothing; instance_windows.go sets it.
var holdInstance = func(string) (release func(), err error) { return func() {}, nil }

// instanceName is the name of the marker of the desktop app that runs on the
// settings file cfgPath: the same for every spelling of the same path.
func instanceName(cfgPath string) string {
	p, err := filepath.Abs(cfgPath)
	if err != nil {
		p = cfgPath
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	sum := sha256.Sum256([]byte(p))
	return `Local\agentlink-app-` + hex.EncodeToString(sum[:16])
}

// alreadyRunning reports whether this start should leave the app to the
// instance that runs already: not for the instance an update started
// (restarted), which waits for the old one to quit.
func alreadyRunning(restarted bool, cfgPath string) bool {
	return !restarted && instanceRunning(instanceName(cfgPath))
}
