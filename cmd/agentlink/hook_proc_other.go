//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether process pid still runs; pid 1 means the
// parent is gone (the waiter was re-parented to init).
func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
