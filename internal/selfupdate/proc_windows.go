package selfupdate

import (
	"context"
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideFile sets FILE_ATTRIBUTE_HIDDEN so the ".<base>.old" leftover of an
// update does not sit visibly next to the executable until the next start
// sweeps it: Windows keeps it locked while its process runs.
func hideFile(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_HIDDEN)
}

const createNoWindow = 0x08000000

// Start runs exe with args as a process that outlives this one: its own
// process group and, when allowed, outside a job object this process runs in,
// so the relaunched app survives this one exiting.
func Start(exe string, args []string) error {
	start := func(breakaway bool) error {
		flags := uint32(createNoWindow | windows.CREATE_NEW_PROCESS_GROUP)
		if breakaway {
			flags |= windows.CREATE_BREAKAWAY_FROM_JOB
		}
		// The relaunched app outlives this one, so no context can end it.
		cmd := exec.CommandContext(context.Background(), exe, args...) // #nosec G204 -- this program's own path and argv
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	err := start(true)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) { // the job forbids breakaway
		err = start(false)
	}
	return err
}
