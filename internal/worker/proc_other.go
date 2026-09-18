//go:build !windows

package worker

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func prepare(*exec.Cmd) {}

// detach puts cmd in its own process group, so a signal to the app's group
// does not reach it and killTree can take down its children.
func detach(cmd *exec.Cmd, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree kills the process group pid leads.
func killTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// procStart returns the start time of a live process (Linux: clock ticks
// since boot from /proc; 0 where that is not available).
func procStart(pid int) (int64, error) {
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, err
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, nil
	}
	s := string(data)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		// Fields after "(comm)": state is field 3, starttime field 22.
		if f := strings.Fields(s[i+1:]); len(f) > 19 {
			v, _ := strconv.ParseInt(f[19], 10, 64)
			return v, nil
		}
	}
	return 0, nil
}

// attach watches a process this app did not start by polling; its exit code
// is not available (-1).
func attach(pid int, start int64) (*proc, bool) {
	if st, err := procStart(pid); err != nil || st != start {
		return nil, false
	}
	p := &proc{pid: pid, start: start, done: make(chan struct{}), code: -1}
	go func() {
		defer close(p.done)
		for {
			time.Sleep(250 * time.Millisecond)
			if st, err := procStart(pid); err != nil || st != start {
				return
			}
		}
	}()
	return p, true
}

func breakawayDenied(error) bool { return false }
