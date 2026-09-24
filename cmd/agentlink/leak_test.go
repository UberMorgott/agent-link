package main

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// The hook tests act as an interactive session even when `go test` runs
	// under a headless agent (claude -p, codex exec).
	hookHeadless = func(string) bool { return false }
	goleak.VerifyTestMain(m)
}
