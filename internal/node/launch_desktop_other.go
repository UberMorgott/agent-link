//go:build !windows

package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func hideWindow(*exec.Cmd) {}

// protocolRegistered: desktop apps are opened on Windows only.
func protocolRegistered(string) bool { return false }

func openURL(context.Context, string) error { return errors.New("windows only") }

// killTree ends p (desktop launches run on Windows only).
func killTree(_ context.Context, p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
