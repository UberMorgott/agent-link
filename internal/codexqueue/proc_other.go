//go:build !windows

package codexqueue

import "os/exec"

func hide(*exec.Cmd) {}
