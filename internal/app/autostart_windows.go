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
	// approvedKey is where Task Manager and Settings > Startup apps record an
	// entry the user switched off without deleting it.
	approvedKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
)

// setAutostart adds or removes the per-user Run entry pointing at this
// executable. Turning it on also clears a "disabled" mark Task Manager left,
// or Windows would keep skipping the entry.
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
	if err := k.SetStringValue(runValueName, runCommand(exe)); err != nil {
		return err
	}
	a, err := registry.OpenKey(registry.CURRENT_USER, approvedKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	if err := a.DeleteValue(runValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// autostartEnabled reports whether Windows starts agentlink at sign-in: the
// Run entry exists and Task Manager has not switched it off.
func autostartEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = k.Close() }()
	if _, _, err := k.GetStringValue(runValueName); errors.Is(err, registry.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	// Without a StartupApproved mark nothing switched the entry off.
	if a, err := registry.OpenKey(registry.CURRENT_USER, approvedKey, registry.QUERY_VALUE); err == nil {
		defer func() { _ = a.Close() }()
		if v, _, err := a.GetBinaryValue(runValueName); err == nil && approvedDisabled(v) {
			return false, nil
		}
	}
	return true, nil
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
	return true, k.SetStringValue(runValueName, runCommand(exe))
}
