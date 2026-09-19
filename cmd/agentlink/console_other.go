//go:build !windows

package main

// leaveConsole keeps the terminal elsewhere: the app runs in the foreground.
func leaveConsole([]string) bool { return false }
