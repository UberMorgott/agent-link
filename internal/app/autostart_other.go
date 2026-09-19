//go:build !windows

package app

import "errors"

func setAutostart(enable bool) error {
	if enable {
		return errors.New("autostart is only supported on Windows")
	}
	return nil
}

// autostartEnabled is always false: autostart is Windows-only.
func autostartEnabled() (bool, error) { return false, nil }

// MigrateAutostart is a no-op: autostart is Windows-only.
func MigrateAutostart(string) (bool, error) { return false, nil }
