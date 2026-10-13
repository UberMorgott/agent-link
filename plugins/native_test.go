package plugins

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// nativeFiles are what the claude CLI reads of the plugin's hooks module
// (aegis.testinputs declares the same): its manifest and hook file, the
// module, its contract and its tests.
var nativeFiles = []string{
	"agent-link/.claude-plugin/plugin.json", "agent-link/hooks/claude-hooks.json",
	"agent-link/hooks/register.ts", "agent-link/hooks/node.ts", "agent-link/hooks/tools.ts", "agent-link/hooks/tools.gen.ts",
	"agent-link/types/index.d.ts",
	"agent-link/tests/node.ts", "agent-link/tests/agents.test.ts", "agent-link/tests/delivery.test.ts", "agent-link/tests/handover.test.ts",
	"agent-link/tests/session.test.ts", "agent-link/tests/tools.test.ts",
}

// TestNativeModule runs the checks of Claude Code's in-process hooks module
// (agent-link/hooks/register.ts) with the claude CLI, so go test (and the
// commit gate) covers them: `claude plugin validate` reads the manifest and
// the module's source as the engine will, `claude plugin test` runs
// tests/*.test.ts against the engine. Neither starts a session or needs a
// login.
func TestNativeModule(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	// A Claude Code without hooks modules has no `plugin test` and rejects the
	// manifest's module keys (types): the probe is the subcommand's own help,
	// since an unknown subcommand prints the parent's. Oldest build this repo
	// verified the module on: 2.1.295.
	help, err := exec.CommandContext(t.Context(), claude, "plugin", "test", "--help").CombinedOutput() //nolint:gosec // G204: the claude CLI's own help
	if err != nil || !bytes.Contains(help, []byte("claude plugin test")) {
		t.Skipf("this claude has no hooks modules (no `claude plugin test`; verified from 2.1.295): %v", err)
	}
	for _, f := range nativeFiles {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []string{"validate", "test"} {
		t.Run(sub, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), claude, "plugin", sub, "agent-link").CombinedOutput() //nolint:gosec // G204: the claude CLI on the plugin's folder
			if err != nil {
				t.Fatalf("claude plugin %s: %v\n%s", sub, err, out)
			}
		})
	}
}
