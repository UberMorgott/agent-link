package app

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/UberMorgott/agent-link/internal/settings"
)

// ErrNoAutostart: this instance uses a settings file other than the default,
// which the Run entry (the executable without flags) would not load.
var ErrNoAutostart = errors.New("autostart is only available with the default settings file")

// Autostart reports whether the app starts at sign-in as Windows has it
// (the saved setting when that cannot be read) and whether this instance can
// change it.
func (a *App) Autostart() (on, available bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autostartLocked(), a.SetAutostart != nil
}

func (a *App) autostartLocked() bool {
	if a.AutostartState != nil {
		on, err := a.AutostartState()
		if err == nil {
			return on
		}
		a.log.Warn("autostart state", "err", err)
	}
	return a.s.Autostart
}

// SetAutostartNow switches starting at sign-in on or off right away (the tray
// checkbox) and saves the choice, without restarting the node.
func (a *App) SetAutostartNow(on bool) error {
	err := a.setAutostartNow(on)
	if err == nil {
		a.events.publish("settings")
	}
	return err
}

func (a *App) setAutostartNow(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.SetAutostart == nil {
		return ErrNoAutostart
	}
	if err := a.SetAutostart(on); err != nil {
		return err
	}
	s := a.s
	s.Autostart = on
	if a.configured {
		if err := settings.Save(a.path, s); err != nil {
			return err
		}
	}
	a.s = s
	return nil
}

// stableExe is the executable an autostart entry must start for exe: an
// update leaves the replaced or downloaded file as ".<name>.old" or
// ".<name>.new" next to it (selfupdate.OldPath), and those are only
// transient, so they map back to <name>.
func stableExe(exe string) string {
	dir, base := filepath.Split(exe)
	for _, ext := range []string{".old", ".new"} {
		if name := strings.TrimSuffix(base, ext); len(name) > 1 && name != base && name[0] == '.' {
			return filepath.Join(dir, name[1:])
		}
	}
	return exe
}

// runCommand is the Run entry's value for exe: the stable path, quoted so a
// folder with spaces starts the right program.
func runCommand(exe string) string { return `"` + stableExe(exe) + `"` }

// approvedDisabled reports a StartupApproved value that marks the entry
// switched off (Task Manager's "Startup apps" writes 03 00 … for off and
// 02 00 … for on; an odd first byte means disabled).
func approvedDisabled(v []byte) bool { return len(v) > 0 && v[0]&1 == 1 }
