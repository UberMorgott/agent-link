package main

import (
	"bytes"
	"encoding/json"
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

// stalePlugin copies the plugin's launchers and .mcp.json into a temporary
// plugin root whose bin also holds a broken agentlink.exe: a leftover of an
// older version that a plugin install copied along (issue #31). Running it
// fails, so a launcher that picks it cannot pass the tests. Returns the root.
func stalePlugin(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "plugins", "agent-link"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".mcp.json", filepath.Join("bin", "agentlink"), filepath.Join("bin", "agentlink.cmd")} {
		data, err := os.ReadFile(filepath.Join(src, name)) //nolint:gosec // G304: the repository's plugin files
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "agentlink.exe"), []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestLauncher runs the plugin's launcher (bin/agentlink.cmd on Windows, the
// POSIX bin/agentlink elsewhere) through each way of finding agentlink. A
// stale agentlink.exe next to the launcher is never run, even when the
// launcher's folder is first on PATH as the MCP server puts it.
func TestLauncher(t *testing.T) {
	bin := filepath.Join(stalePlugin(t), "bin")
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
		{"path past own folder", map[string]string{"APPDATA": empty, "PATH": bin + string(os.PathListSeparator) + filepath.Dir(onPath)}},
	} {
		if out, errOut, code := run(c.env); code != 7 || out != want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 7, %q", c.name, code, out, errOut, want)
		}
	}
	// An explicit AGENTLINK_EXE never falls back; nothing found is an error that says how to fix it.
	if _, errOut, code := run(map[string]string{"AGENTLINK_EXE": filepath.Join(empty, exeName), "APPDATA": withMarker}); code != 1 || !strings.Contains(errOut, "AGENTLINK_EXE is set") {
		t.Errorf("missing AGENTLINK_EXE: exit %d, stderr %q", code, errOut)
	}
	for _, path := range []string{emptyPath, bin} {
		if _, errOut, code := run(map[string]string{"APPDATA": empty, "PATH": path}); code != 1 || !strings.Contains(errOut, "agentlink desktop app") {
			t.Errorf("not found, PATH %s: exit %d, stderr %q", path, code, errOut)
		}
	}
}

// TestMCPConfig runs the plugin's .mcp.json command line the way Claude Code
// and Codex do (cmd with the plugin root in PLUGIN_ROOT or CLAUDE_PLUGIN_ROOT)
// and checks it reaches agentlink through the launcher, never through a stale
// agentlink.exe the install copied into the plugin's bin (issue #31).
func TestMCPConfig(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the MCP command line is cmd.exe")
	}
	root := stalePlugin(t)
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(filepath.Join(root, ".mcp.json")) //nolint:gosec // G304: the test's copy of .mcp.json
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	server, ok := cfg.MCPServers["agentlink"]
	if !ok || server.Command != "cmd" {
		t.Fatalf(".mcp.json agentlink server = %+v", server)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The launcher runs AGENTLINK_EXE with the MCP server's arguments; this
	// wrapper turns that into a run of TestLauncherHelper.
	wrapper := filepath.Join(t.TempDir(), "agentlink.cmd")
	if err := os.WriteFile(wrapper, []byte("@\""+self+"\" -test.run=TestLauncherHelper$ -- %*\r\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rootVar := range []string{"PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT"} {
		cmd := exec.CommandContext(t.Context(), os.Getenv("ComSpec"), server.Args...) //nolint:gosec // G204: the repository's MCP command line
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			switch strings.ToUpper(k) {
			case "AGENTLINK_EXE", "PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT":
				continue
			}
			cmd.Env = append(cmd.Env, kv)
		}
		cmd.Env = append(cmd.Env, launcherHelperEnv+"=1", "AGENTLINK_EXE="+wrapper, rootVar+"="+root)
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
		if want := "mcp|{}"; code != 7 || out.String() != want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 7, %q", rootVar, code, out.String(), errOut.String(), want)
		}
	}
}
