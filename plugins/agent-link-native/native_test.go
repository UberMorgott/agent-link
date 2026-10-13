// Package native holds the spike of agent-link's Claude Code integration as an
// in-process hooks module (hooks/register.ts); its test runs the module's own
// checks with the claude CLI, so go test (and the commit gate) covers them.
package native

import (
	"os"
	"os/exec"
	"testing"
)

// TestMod runs `claude plugin validate` (the manifest and the module's source,
// read as the engine will) and `claude plugin test` (tests/*.test.ts against
// the engine) on this folder. Neither starts a session or needs a login.
func TestMod(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	// What the claude CLI reads here (aegis.testinputs declares the same).
	for _, f := range []string{
		".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", "types/index.d.ts",
		"tests/node.ts", "tests/ack.test.ts", "tests/append.test.ts", "tests/wake.test.ts",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []string{"validate", "test"} {
		t.Run(sub, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), claude, "plugin", sub, ".").CombinedOutput() //nolint:gosec // G204: the claude CLI on this folder
			if err != nil {
				t.Fatalf("claude plugin %s: %v\n%s", sub, err, out)
			}
		})
	}
}
