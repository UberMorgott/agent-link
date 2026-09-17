// Command fakeagent stands in for an agent CLI in the e2e scripts. It prints
// "echo: " followed by the prompt read from stdin, except for a prompt
// "tree MARKER": then it starts a child "fakeagent sleep MARKER" and blocks,
// like an agent shim with a long-running child, so a script can look for
// leftover processes by MARKER on their command line.
package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "sleep" {
		time.Sleep(10 * time.Minute)
		return
	}
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(1)
	}
	if marker, ok := strings.CutPrefix(strings.TrimSpace(string(in)), "tree "); ok {
		if err := exec.Command(os.Args[0], "sleep", marker).Start(); err != nil {
			os.Exit(1)
		}
		time.Sleep(10 * time.Minute)
		return
	}
	_, _ = os.Stdout.WriteString("echo: " + string(in))
}
