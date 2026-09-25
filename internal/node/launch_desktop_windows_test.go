package node

import (
	"context"
	"os"
	"testing"
)

// TestDesktopLauncherLive runs a real first turn and opens it in the desktop
// app, only when AGENTLINK_E2E_DESKTOP_DIR names a scratch folder: provider
// AGENTLINK_E2E_DESKTOP_PROVIDER (default claude), resuming
// AGENTLINK_E2E_DESKTOP_RESUME when set, with prompt
// AGENTLINK_E2E_DESKTOP_PROMPT.
func TestDesktopLauncherLive(t *testing.T) {
	dir := os.Getenv("AGENTLINK_E2E_DESKTOP_DIR")
	if dir == "" {
		t.Skip("no desktop folder given")
	}
	spec := LaunchSpec{Provider: os.Getenv("AGENTLINK_E2E_DESKTOP_PROVIDER"), Folder: dir,
		ResumeID: os.Getenv("AGENTLINK_E2E_DESKTOP_RESUME"), Prompt: os.Getenv("AGENTLINK_E2E_DESKTOP_PROMPT")}
	if spec.Provider == "" {
		spec.Provider = ProviderClaude
	}
	l := DesktopLauncher{Version: "e2e"}
	if !l.Direct(spec.Provider) {
		t.Fatalf("no desktop app for %s", spec.Provider)
	}
	err := l.Run(context.Background(), spec, func(id string) { t.Logf("started session %s", id) })
	if err != nil {
		t.Fatal(err)
	}
}
