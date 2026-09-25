package node

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

const createNoWindow = 0x08000000

// hideWindow keeps a headless agent from opening a console window (the tray
// app has none).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// protocolRegistered reports whether the URL protocol scheme (the desktop
// app's deep links) has a handler, for this user or the machine.
func protocolRegistered(scheme string) bool {
	for _, k := range []struct {
		root registry.Key
		path string
	}{{registry.CURRENT_USER, `Software\Classes\` + scheme}, {registry.CLASSES_ROOT, scheme}} {
		key, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		_, _, err = key.GetStringValue("URL Protocol")
		_ = key.Close()
		if err == nil {
			return true
		}
	}
	return false
}

// killTree ends p and every process it started (taskkill /T), p alone when
// taskkill fails.
func killTree(ctx context.Context, p *os.Process) error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(p.Pid)) //nolint:gosec // G204: fixed program, a pid
	hideWindow(kill)
	if err := kill.Run(); err != nil {
		return p.Kill()
	}
	return nil
}

// openURL opens a deep link in its registered app.
func openURL(ctx context.Context, u string) error {
	return exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", u).Run() //nolint:gosec // G204: fixed program, a deep link built by DeepLink
}
