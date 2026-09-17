//go:build !windows

package main

import (
	"fmt"
	"os"
)

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "agentlink:", err)
	os.Exit(1)
}
