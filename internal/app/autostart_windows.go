package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/UberMorgott/agent-link/internal/selfupdate"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "agentlink"
)

// setAutostart adds or removes the per-user Run entry pointing at this executable.
func setAutostart(enable bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	if !enable {
		if err := k.DeleteValue(runValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValueName, `"`+exe+`"`)
}

// MigrateAutostart points an autostart entry that starts agentlink-tray.exe,
// the desktop app of older releases, at exe: the one agentlink executable
// that replaces it. It reports whether it changed the entry.
func MigrateAutostart(exe string) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = k.Close() }()
	cur, _, err := k.GetStringValue(runValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !selfupdate.LegacyName(filepath.Base(strings.Trim(strings.TrimSpace(cur), `"`))) {
		return false, nil
	}
	return true, k.SetStringValue(runValueName, `"`+exe+`"`)
}
