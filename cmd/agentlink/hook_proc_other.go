//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// agentCommandLine is the command line of the agent process (claude, codex)
// this hook runs under, read from /proc: the nearest ancestor whose command
// names client. nil when there is none or /proc is missing.
func agentCommandLine(client string) []string {
	pid := os.Getppid()
	for range 10 {
		if pid <= 1 {
			return nil
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return nil
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		for _, a := range args[:min(len(args), 2)] {
			if strings.TrimSuffix(filepath.Base(a), ".js") == client {
				return args
			}
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil
		}
		// pid (comm) state ppid ...: comm may hold spaces, so parse after ')'.
		i := strings.LastIndexByte(string(stat), ')')
		f := strings.Fields(string(stat[i+1:]))
		if i < 0 || len(f) < 2 {
			return nil
		}
		if pid, err = strconv.Atoi(f[1]); err != nil {
			return nil
		}
	}
	return nil
}

// processAlive reports whether process pid still runs; pid 1 means the
// parent is gone (the waiter was re-parented to init).
func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
