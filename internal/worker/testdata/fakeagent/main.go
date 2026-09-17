// Command fakeagent stands in for an agent CLI in scripts/e2e-worker.ps1: it
// prints "echo: " followed by the prompt read from stdin.
package main

import (
	"io"
	"os"
)

func main() {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(1)
	}
	_, _ = os.Stdout.WriteString("echo: " + string(in))
}
