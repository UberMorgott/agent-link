package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOverlayHolds(t *testing.T) {
	local := t.TempDir()
	if overlayHolds(local, "agentlink", "probe") {
		t.Fatal("no Packages folder holds nothing")
	}
	dir := filepath.Join(local, "Packages", "Claude_x", "LocalCache", "Roaming", "agentlink")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(local, "Packages", "Other_y"), 0o700); err != nil {
		t.Fatal(err)
	}
	if overlayHolds(local, "agentlink", "probe") {
		t.Fatal("found a file that is not there")
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !overlayHolds(local, "agentlink", "probe") {
		t.Fatal("private copy not found")
	}
}

// A real folder is not virtualized; a folder outside AppData is never probed.
func TestSandboxed(t *testing.T) {
	appData, local := t.TempDir(), t.TempDir()
	cfg := filepath.Join(appData, "agentlink")
	for _, c := range []struct{ dir, appData, local string }{
		{cfg, appData, local},
		{t.TempDir(), appData, local},
		{cfg, "", local},
	} {
		if in, err := sandboxed(c.dir, c.appData, c.local); in || err != nil {
			t.Fatalf("sandboxed(%q, %q, %q) = %v, %v", c.dir, c.appData, c.local, in, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(cfg, sandboxedPrefix+"*")); len(left) != 0 {
		t.Fatalf("probe left behind: %v", left)
	}
}

func TestOutsideCommandLine(t *testing.T) {
	got := outsideCommandLine(`C:\Program Files\agentlink\agentlink.exe`, []string{"-config", `C:\a b\config.json`, "-" + sandboxFlag})
	want := `"C:\Program Files\agentlink\agentlink.exe" -config "C:\a b\config.json" -unsandboxed`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if args := withoutFlag([]string{"--unsandboxed", "-x", "-unsandboxed"}, sandboxFlag); !slices.Equal(args, []string{"-x"}) {
		t.Fatalf("withoutFlag = %v", args)
	}
}
