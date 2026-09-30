// Package scripts holds the repository's PowerShell release scripts; its test
// runs their Pester-less test scripts, so go test (and the commit gate) covers
// them.
package scripts

import (
	"os/exec"
	"testing"
)

// TestScripts runs each *.test.ps1 of this folder with pwsh; a script exits
// with the number of its failed cases.
func TestScripts(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed")
	}
	for _, script := range []string{"swap.test.ps1", "release.test.ps1"} {
		t.Run(script, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-File", script).CombinedOutput() //nolint:gosec // G204: this folder's own test scripts
			if err != nil {
				t.Fatalf("%s: %v\n%s", script, err, out)
			}
		})
	}
}
