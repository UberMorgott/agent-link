package node

import (
	"context"
	"os"
	"testing"
)

// TestTerminalLauncherLive opens a real session in Windows Terminal, only
// when AGENTLINK_E2E_LAUNCH_DIR names a scratch folder: provider
// AGENTLINK_E2E_LAUNCH_PROVIDER (default claude), resuming
// AGENTLINK_E2E_LAUNCH_RESUME when set, with prompt AGENTLINK_E2E_LAUNCH_PROMPT.
func TestTerminalLauncherLive(t *testing.T) {
	dir := os.Getenv("AGENTLINK_E2E_LAUNCH_DIR")
	if dir == "" {
		t.Skip("no launch folder given")
	}
	spec := LaunchSpec{Provider: os.Getenv("AGENTLINK_E2E_LAUNCH_PROVIDER"), Folder: dir,
		ResumeID: os.Getenv("AGENTLINK_E2E_LAUNCH_RESUME"), Prompt: os.Getenv("AGENTLINK_E2E_LAUNCH_PROMPT")}
	if spec.Provider == "" {
		spec.Provider = ProviderClaude
	}
	t.Logf("launching %q", LaunchCommand(spec))
	if err := (TerminalLauncher{}).Launch(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}
