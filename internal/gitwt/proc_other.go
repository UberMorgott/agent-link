//go:build !windows

package gitwt

import "os/exec"

func hide(*exec.Cmd) {}
