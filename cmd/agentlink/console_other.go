//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// isConsole reports whether f is a terminal.
func isConsole(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// consoleInputCP is the console's input code page: none here (0).
func consoleInputCP() uint32 { return 0 }

// leaveConsole keeps the terminal elsewhere: the app runs in the foreground.
func leaveConsole([]string) bool { return false }

// hideConsole: no console windows here.
func hideConsole(*exec.Cmd) {}
