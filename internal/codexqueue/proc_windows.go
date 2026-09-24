package codexqueue

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hide keeps codex from opening a console window (the tray app has none).
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
