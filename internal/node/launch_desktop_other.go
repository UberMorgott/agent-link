//go:build !windows

package node

import (
	"context"
	"errors"
	"os/exec"
)

func hideWindow(*exec.Cmd) {}

// protocolRegistered: desktop apps are opened on Windows only.
func protocolRegistered(string) bool { return false }

func openURL(context.Context, string) error { return errors.New("windows only") }
