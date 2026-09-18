package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent installed by npm is a .cmd shim. It must run by its absolute path,
// get the fixed arguments and read the prompt from stdin; shell metacharacters
// in the prompt stay data and never reach cmd.exe's command line.
func TestRunnerRunsCmdShim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "npm dir") // a space, as in C:\Users\First Last
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "agent.cmd")
	script := "@echo off\r\nif not \"%~1\"==\"exec\" exit /b 3\r\necho args:%~1 %~2\r\nfindstr \"^\"\r\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	prompt := "question & echo x > " + marker + " | %PATH% \"quoted\" ^caret"
	out, err := Command{Name: shim, Args: []string{"exec", "--sandbox"}}.Runner()(context.Background(), t.TempDir(), prompt)
	if err != nil {
		t.Fatalf("run %s: %v", shim, err)
	}
	if !strings.Contains(out, "args:exec --sandbox") {
		t.Fatalf("arguments not passed: %q", out)
	}
	if !strings.Contains(out, prompt) {
		t.Fatalf("prompt not read from stdin verbatim: %q", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the prompt was executed by the shell")
	}
}
