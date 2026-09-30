package agenthook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Codex gets the plugin with its own commands, the marketplace only when it
// is not configured; any agent-link entry, even a disabled one, installs
// nothing, and a config that does not parse installs nothing either.
func TestCodexInstallCommands(t *testing.T) {
	add := []string{"codex", "plugin", "add", "agent-link@agent-link"}
	market := []string{"codex", "plugin", "marketplace", "add", "UberMorgott/agent-link"}
	for name, tc := range map[string]struct {
		config string // "-" leaves config.toml out
		want   [][]string
	}{
		"no config":      {"-", [][]string{market, add}},
		"other plugins":  {"[marketplaces.caveman]\nsource_type = \"git\"\n[plugins.\"caveman@caveman\"]\nenabled = true\n", [][]string{market, add}},
		"has market":     {"[marketplaces.agent-link]\nsource_type = \"git\"\nsource = \"https://github.com/UberMorgott/agent-link.git\"\n", [][]string{add}},
		"installed":      {"[plugins.\"agent-link@agent-link\"]\nenabled = true\n", nil},
		"disabled":       {"[plugins.\"agent-link@mine\"]\nenabled = false\n", nil},
		"broken config":  {"[plugins.\"x\"\n", nil},
		"similar plugin": {"[plugins.\"agent-link-x@agent-link\"]\nenabled = true\n[marketplaces.agent-link]\n", [][]string{add}},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if tc.config != "-" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_HOME", home)
			if got := CodexInstallCommands(); !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("CodexInstallCommands() = %q, want %q", got, tc.want)
			}
		})
	}
}

// InstallCodexPlugin runs the commands through the resolved codex program,
// stops at the first failure with the command and its output, and runs
// nothing when the plugin is listed or codex is not found.
func TestInstallCodexPlugin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	var ran [][]string
	ok := func(_ context.Context, program string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{program}, args...))
		return nil, nil
	}
	if did, err := InstallCodexPlugin(context.Background(), "", ok); did || err == nil || ran != nil {
		t.Fatalf("no codex: ran=%v err=%v cmds=%q", did, err, ran)
	}
	if did, err := InstallCodexPlugin(context.Background(), `C:\bin\codex.exe`, ok); !did || err != nil {
		t.Fatalf("install: ran=%v err=%v", did, err)
	}
	want := [][]string{{`C:\bin\codex.exe`, "plugin", "marketplace", "add", "UberMorgott/agent-link"}, {`C:\bin\codex.exe`, "plugin", "add", "agent-link@agent-link"}}
	if !slices.EqualFunc(ran, want, slices.Equal) {
		t.Fatalf("ran %q", ran)
	}
	ran = nil
	fail := func(_ context.Context, program string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{program}, args...))
		return []byte("network down\n"), errors.New("exit status 1")
	}
	if did, err := InstallCodexPlugin(context.Background(), "codex", fail); !did || err == nil || len(ran) != 1 ||
		!strings.Contains(err.Error(), "marketplace add UberMorgott/agent-link") || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("failure: ran=%v err=%v cmds=%q", did, err, ran)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[plugins.\"agent-link@agent-link\"]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	if did, err := InstallCodexPlugin(context.Background(), "codex", ok); did || err != nil || ran != nil {
		t.Fatalf("listed plugin: ran=%v err=%v cmds=%q", did, err, ran)
	}
}
