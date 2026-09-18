// Command fakeagent stands in for an agent CLI in the e2e scripts. It prints
// "echo: " followed by the prompt read from stdin, except for a prompt
// "tree MARKER": then it starts a child "fakeagent sleep MARKER" and blocks,
// like an agent shim with a long-running child, so a script can look for
// leftover processes by MARKER on their command line. A prompt
// "slow SECONDS LOGFILE LABEL" appends LABEL to LOGFILE (one line per run),
// sleeps, then echoes, so a script can count runs.
//
// Two prompts speak Claude's stream-json (the tray runs a handler_command in
// its handler's output format): "stream SECONDS LOGFILE LABEL" prints a Read
// tool call every 500ms for SECONDS, then the result "done LABEL";
// "stall LOGFILE LABEL" prints one tool call and then hangs. Both append
// "start LABEL <unix ms>" and, when they finish, "end LABEL <unix ms>" to
// LOGFILE, so a script can see which runs overlapped.
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
	if f := strings.Fields(prompt); len(f) == 4 && f[0] == "stream" {
		secs, err := strconv.Atoi(f[1])
		if err != nil || stamp(f[2], "start", f[3]) != nil {
			os.Exit(1)
		}
		for i := 0; i < secs*2; i++ {
			toolCall("Read", f[3]+"-"+strconv.Itoa(i)+".md")
			time.Sleep(500 * time.Millisecond)
		}
		_ = stamp(f[2], "end", f[3])
		_, _ = os.Stdout.WriteString(`{"type":"result","subtype":"success","is_error":false,"result":"done ` + f[3] + `"}` + "\n")
		return
	}
	if f := strings.Fields(prompt); len(f) == 3 && f[0] == "stall" {
		if stamp(f[1], "start", f[2]) != nil {
			os.Exit(1)
		}
		toolCall("Read", f[2]+".md")
		time.Sleep(10 * time.Minute)
		return
	}
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

// toolCall prints one Claude stream-json assistant event with a tool call.
func toolCall(name, file string) {
	_, _ = os.Stdout.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"` + name +
		`","input":{"file_path":"` + file + `"}}]}}` + "\n")
}

func stamp(path, what, label string) error {
	return appendLine(path, what+" "+label+" "+strconv.FormatInt(time.Now().UnixMilli(), 10))
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
