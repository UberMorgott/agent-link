//go:build !windows

package worker

import "os/exec"

func prepare(*exec.Cmd) {}
