//go:build !windows

package app

import "errors"

func setAutostart(enable bool) error {
	if enable {
		return errors.New("autostart is only supported on Windows")
	}
	return nil
}
