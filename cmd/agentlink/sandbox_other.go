//go:build !windows

package main

// leaveSandbox: AppData virtualization by packaged apps is Windows only.
func leaveSandbox(string, []string, bool, bool) (bool, error) { return false, nil }
