// Command fakeagent stands in for an agent CLI in the e2e scripts. It prints
// "echo: " followed by the prompt read from stdin, except for a prompt
// "tree MARKER": then it starts a child "fakeagent sleep MARKER" and blocks,
// like an agent shim with a long-running child, so a script can look for
// leftover processes by MARKER on their command line. A prompt
// "slow SECONDS LOGFILE LABEL" appends LABEL to LOGFILE (one line per run),
// sleeps, then echoes, so a script can count runs.
package main

import (
	"io"
	"os"
	"os/exec"
	"strconv"
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
	prompt := strings.TrimSpace(string(in))
	if f := strings.Fields(prompt); len(f) == 4 && f[0] == "slow" {
		secs, err := strconv.Atoi(f[1])
		if err != nil || appendLine(f[2], f[3]) != nil {
			os.Exit(1)
		}
		time.Sleep(time.Duration(secs) * time.Second)
		_, _ = os.Stdout.WriteString("echo: " + string(in))
		return
	}
	if marker, ok := strings.CutPrefix(prompt, "tree "); ok {
		if err := exec.Command(os.Args[0], "sleep", marker).Start(); err != nil {
			os.Exit(1)
		}
		time.Sleep(10 * time.Minute)
		return
	}
	_, _ = os.Stdout.WriteString("echo: " + string(in))
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
