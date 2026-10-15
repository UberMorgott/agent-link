package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstanceNameSamePath(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	name := instanceName(cfg)
	if !strings.HasPrefix(name, `Local\agentlink-app-`) || strings.Count(name, `\`) != 1 {
		t.Fatalf("instanceName = %q: want one Local\\ prefix and no other backslash", name)
	}
	if got := instanceName(filepath.Join(dir, "sub", "..", "settings.json")); got != name {
		t.Errorf("unclean spelling: %q, want %q", got, name)
	}
	if runtime.GOOS == "windows" {
		if got := instanceName(strings.ToUpper(cfg)); got != name {
			t.Errorf("other case on Windows: %q, want %q", got, name)
		}
	}
	if got := instanceName(filepath.Join(dir, "other.json")); got == name {
		t.Errorf("another settings file got the same name %q", got)
	}
}

// A start of the app without a command leaves the app to the instance that
// holds the marker of the same settings file, unless an update started it
// (issue #32).
func TestAlreadyRunning(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "settings.json")
	if alreadyRunning(false, cfg) {
		t.Fatal("running before any instance holds the marker")
	}
	release, err := holdInstance(instanceName(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if !alreadyRunning(false, cfg) {
			t.Error("a second start did not find the running instance")
		}
		if alreadyRunning(true, cfg) {
			t.Error("the instance an update started must wait for the old one, not exit")
		}
		if alreadyRunning(false, filepath.Join(t.TempDir(), "settings.json")) {
			t.Error("an instance of other settings counted as running")
		}
	}
	release()
	if alreadyRunning(false, cfg) {
		t.Error("still running after the instance released its marker")
	}
}
