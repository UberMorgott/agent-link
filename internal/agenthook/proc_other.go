//go:build !windows

package agenthook

import "os/exec"

func hide(*exec.Cmd) {}
