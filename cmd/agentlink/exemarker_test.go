package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteExeMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentlink")
	for _, exe := range []string{`C:\Program Files (x86)\agentlink\agentlink.exe`, `D:\a\agentlink.exe`} {
		if err := writeExeMarker(dir, exe); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Clean(filepath.Join(dir, exeMarkerName)))
		if err != nil || string(got) != exe {
			t.Fatalf("marker = %q, %v; want %q", got, err, exe)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(left) > 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// launcherHelperEnv makes this test binary act as agentlink for the launcher
// tests: TestLauncherHelper prints its arguments and stdin, exits 7.
const launcherHelperEnv = "AGENTLINK_TEST_LAUNCHER_HELPER"

func TestLauncherHelper(t *testing.T) {
	if os.Getenv(launcherHelperEnv) != "1" {
		t.Skip("run by the launcher tests")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	in, _ := io.ReadAll(os.Stdin)
	fmt.Printf("%s|%s", strings.Join(args, ","), in)
	os.Exit(7)
}

// TestLauncher runs the plugin's launcher (bin/agentlink.cmd on Windows, the
// POSIX bin/agentlink elsewhere) through each way of finding agentlink.
func TestLauncher(t *testing.T) {
	bin, err := filepath.Abs(filepath.Join("..", "..", "plugins", "agent-link", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exeName := "agentlink"
	if runtime.GOOS == "windows" {
		exeName = "agentlink.exe"
	}
	onPath := filepath.Join(t.TempDir(), exeName)
	if data, err := os.ReadFile(self); err != nil || os.WriteFile(onPath, data, 0o700) != nil { //nolint:gosec // a test executable
		t.Fatalf("copy the test binary: %v", err)
	}
	withMarker, empty, emptyPath := t.TempDir(), t.TempDir(), t.TempDir()
	if err := writeExeMarker(filepath.Join(withMarker, "agentlink"), self); err != nil {
		t.Fatal(err)
	}
	run := func(env map[string]string) (string, string, int) {
		t.Helper()
		helper := []string{"-test.run=TestLauncherHelper$", "--", "hook", "claude", "a b"}
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(t.Context(), os.Getenv("ComSpec"), append([]string{"/d", "/c", filepath.Join(bin, "agentlink.cmd")}, helper...)...) //nolint:gosec // G204: the repository's launcher, test arguments
		} else {
			cmd = exec.CommandContext(t.Context(), "/bin/sh", append([]string{filepath.Join(bin, "agentlink")}, helper...)...) //nolint:gosec // G204: the repository's launcher, test arguments
		}
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			switch strings.ToUpper(k) {
			case "AGENTLINK_EXE", "APPDATA", "PATH", "XDG_CONFIG_HOME":
				continue
			}
			cmd.Env = append(cmd.Env, kv)
		}
		cmd.Env = append(cmd.Env, launcherHelperEnv+"=1")
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin = strings.NewReader("{}")
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		code := 0
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return out.String(), errOut.String(), code
	}
	const want = "hook,claude,a b|{}"
	for _, c := range []struct {
		name string
		env  map[string]string
	}{
		{"env", map[string]string{"AGENTLINK_EXE": self, "APPDATA": withMarker, "PATH": emptyPath}},
		{"marker", map[string]string{"APPDATA": withMarker, "PATH": emptyPath}},
		{"path", map[string]string{"APPDATA": empty, "PATH": filepath.Dir(onPath)}},
	} {
		if out, errOut, code := run(c.env); code != 7 || out != want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 7, %q", c.name, code, out, errOut, want)
		}
	}
	// An explicit AGENTLINK_EXE never falls back; nothing found is an error that says how to fix it.
	if _, errOut, code := run(map[string]string{"AGENTLINK_EXE": filepath.Join(empty, exeName), "APPDATA": withMarker}); code != 1 || !strings.Contains(errOut, "AGENTLINK_EXE is set") {
		t.Errorf("missing AGENTLINK_EXE: exit %d, stderr %q", code, errOut)
	}
	if _, errOut, code := run(map[string]string{"APPDATA": empty, "PATH": emptyPath}); code != 1 || !strings.Contains(errOut, "agentlink desktop app") {
		t.Errorf("not found: exit %d, stderr %q", code, errOut)
	}
}
