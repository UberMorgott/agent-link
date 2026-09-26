package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/UberMorgott/agent-link/internal/worker"
)

// A binding's auto-open switch of an older file becomes its autonomy: on is
// asked, off (or never set) is off; the next save writes no auto_open.
func TestAutoOpenMigratesToAutonomy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	on, off, unset, kept := newBinding(t, ""), newBinding(t, ""), newBinding(t, ""), newBinding(t, "")
	yes, no := true, false
	on.AutoOpen, off.AutoOpen, kept.AutoOpen, kept.Autonomy = &yes, &no, &no, AutonomyFull
	data, err := json.Marshal(Settings{Version: Version, Node: "alice", Handler: worker.HandlerNone, //nolint:gosec // G117: a test file with made-up project secrets
		Bindings: []ProjectBinding{on, off, unset, kept}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{AutonomyAsked, AutonomyOff, AutonomyOff, AutonomyFull}
	for i, b := range s.Bindings {
		if b.AutoOpen != nil || b.AutonomyOf() != want[i] {
			t.Errorf("binding %d: auto_open %v, autonomy %q, want %q", i, b.AutoOpen, b.Autonomy, want[i])
		}
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Clean(path)); bytes.Contains(got, []byte("auto_open")) || !bytes.Contains(got, []byte(`"autonomy": "asked"`)) {
		t.Fatalf("saved = %s", got)
	}
}

// The hop limit follows the mode unless set: none in full mode, 8 otherwise;
// the budgets have their defaults; out-of-range values are refused.
func TestAutonomyDefaultsAndLimits(t *testing.T) {
	b := newBinding(t, "")
	if b.AutonomyOf() != AutonomyOff || b.MaxAutoDepthOf() != 8 || b.TurnsPerHourOf() != 30 || b.MaxRunMinutesOf() != 240 {
		t.Fatalf("defaults: %+v", b)
	}
	b.Autonomy = AutonomyFull
	if b.MaxAutoDepthOf() != 0 {
		t.Fatal("full mode keeps a hop limit by default")
	}
	five, zero := 5, 0
	b.MaxAutoDepth = &five
	if b.MaxAutoDepthOf() != 5 {
		t.Fatal("a set hop limit is not kept")
	}
	b.Autonomy, b.MaxAutoDepth = AutonomyAsked, &zero
	if b.MaxAutoDepthOf() != 0 {
		t.Fatal("a hop limit of 0 (none) is not kept in asked mode")
	}
	big, neg := MaxMaxAutoDepth+1, -1
	for name, mod := range map[string]func(*ProjectBinding){
		"mode":      func(b *ProjectBinding) { b.Autonomy = "always" },
		"depth":     func(b *ProjectBinding) { b.MaxAutoDepth = &big },
		"neg depth": func(b *ProjectBinding) { b.MaxAutoDepth = &neg },
		"turns":     func(b *ProjectBinding) { b.TurnsPerHour = MaxTurnsPerHour + 1 },
		"run short": func(b *ProjectBinding) { b.MaxRunMinutes = MinMaxRunMinutes - 1 },
		"run long":  func(b *ProjectBinding) { b.MaxRunMinutes = MaxMaxRunMinutes + 1 },
	} {
		c := newBinding(t, "")
		mod(&c)
		var p *Problem
		if err := validateBindings([]ProjectBinding{c}); !errors.As(err, &p) || p.Key != "autonomy" {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := newBinding(t, "")
	ok.Autonomy, ok.MaxAutoDepth, ok.TurnsPerHour, ok.MaxRunMinutes = AutonomyFull, &zero, MaxTurnsPerHour, MinMaxRunMinutes
	if err := validateBindings([]ProjectBinding{ok}); err != nil {
		t.Fatal(err)
	}
}
