package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/UberMorgott/agent-link/internal/selfupdate"
)

// agentlink.exe is a console program: a terminal (pwsh, cmd, an agent's
// shell) then waits for a CLI command, shows its output and gets its exit
// code, none of which holds for a -H=windowsgui build. The desktop app has
// no use for a console and leaves it here.

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procShowWindow            = windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow")
)

// gui is set once the app has no console: fatal then shows a message box.
var gui bool

// leaveConsole detaches the desktop app from its console. A console of its
// own (a double-click, the autostart entry) is hidden and released. A console
// shared with a terminal is left by starting a detached copy with the same
// args; leaveConsole then reports true and this process should exit, so the
// terminal is free again and closing it does not end the app.
func leaveConsole(args []string) bool {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))) // #nosec G103 -- Win32 call with a local buffer
	switch n {
	case 0: // no console at all
		gui = true
		return false
	case 1: // the console Windows opened for this process
		if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
			_, _, _ = procShowWindow.Call(hwnd, 0) // SW_HIDE
		}
		_, _, _ = procFreeConsole.Call()
		gui = true
		return false
	}
	exe, err := os.Executable()
	if err == nil {
		err = selfupdate.Start(exe, args)
	}
	if err != nil {
		// Stay in the terminal rather than not start at all.
		_, _ = fmt.Fprintln(os.Stderr, "agentlink: could not detach from the terminal:", err)
		return false
	}
	_, _ = fmt.Fprintln(os.Stderr, "agentlink: the app runs in the tray")
	return true
}
