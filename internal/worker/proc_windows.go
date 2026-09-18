package worker

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	createNoWindow = 0x08000000
	stillActive    = 259 // GetExitCodeProcess of a running process
)

// prepare hides the agent's console window (the tray app has none) and kills
// the whole process tree on cancel, since agent CLIs spawn children.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.Cancel = func() error { return killTree(cmd.Process.Pid) }
}

// detach makes cmd a process that outlives this one: its own process group
// (a console Ctrl+C or Ctrl+Break sent to the app does not reach it) and, with
// breakaway, outside a job object the app may run in, so closing that job
// does not take the agent down. No console window either way.
func detach(cmd *exec.Cmd, breakaway bool) {
	flags := uint32(createNoWindow | windows.CREATE_NEW_PROCESS_GROUP)
	if breakaway {
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
}

// killTree kills pid and every process it started.
func killTree(pid int) error {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := kill.Run(); err != nil {
		p, perr := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if perr != nil {
			return err
		}
		defer func() { _ = windows.CloseHandle(p) }()
		return windows.TerminateProcess(p, 1)
	}
	return nil
}

// procStart returns the creation time of a live process pid, which tells it
// apart from a later process that reuses the pid.
func procStart(pid int) (int64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return startOf(h)
}

func startOf(h windows.Handle) (int64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return created.Nanoseconds(), nil
}

// attach watches a process this app did not start (or started before a
// restart). It reports false when pid is gone or is now another process. The
// exit code is the real one: the handle keeps it readable after exit.
func attach(pid int, start int64) (*proc, bool) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, false
	}
	var code uint32
	st, err := startOf(h)
	if err != nil || st != start || windows.GetExitCodeProcess(h, &code) != nil || code != stillActive {
		_ = windows.CloseHandle(h)
		return nil, false
	}
	p := &proc{pid: pid, start: start, done: make(chan struct{}), code: -1}
	go func() {
		defer close(p.done)
		defer func() { _ = windows.CloseHandle(h) }()
		if ev, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil || ev != windows.WAIT_OBJECT_0 {
			return
		}
		if windows.GetExitCodeProcess(h, &code) == nil {
			p.code = int(code)
		}
	}()
	return p, true
}

// breakawayDenied reports whether a start failed because the app's job object
// forbids breakaway; the start is then retried without it.
func breakawayDenied(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
