package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

func init() {
	instanceRunning = mutexHeld
	holdInstance = holdMutex
}

// mutexHeld reports whether a process holds the named mutex name. A mutex
// this process may not open (one a shell of another user, such as an agent's
// sandbox account, finds) exists all the same.
func mutexHeld(name string) bool {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, n)
	if err == nil {
		_ = windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// holdMutex creates (or opens) the named mutex name and keeps it until
// release: while any handle is open, mutexHeld finds it. No ownership: an
// updated instance shares it with the old one while that one quits.
func holdMutex(name string) (release func(), err error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, n)
	if h == 0 {
		return nil, err
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}
