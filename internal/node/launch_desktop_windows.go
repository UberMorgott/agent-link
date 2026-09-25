package node

import (
	"context"
	"os/exec"
	"syscall"

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

// openURL opens a deep link in its registered app.
func openURL(ctx context.Context, u string) error {
	return exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", u).Run() //nolint:gosec // G204: fixed program, a deep link built by DeepLink
}
