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
// out-of-range values are refused.
func TestAutonomyDefaultsAndLimits(t *testing.T) {
	b := newBinding(t, "")
	if b.AutonomyOf() != AutonomyOff || b.MaxAutoDepthOf() != 8 {
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
	} {
		c := newBinding(t, "")
		mod(&c)
		var p *Problem
		if err := validateBindings([]ProjectBinding{c}); !errors.As(err, &p) || p.Key != "autonomy" {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := newBinding(t, "")
	ok.Autonomy, ok.MaxAutoDepth = AutonomyFull, &zero
	if err := validateBindings([]ProjectBinding{ok}); err != nil {
		t.Fatal(err)
	}
}

// The budget fields of an older file (turns_per_hour, max_run_minutes), even
// out of their old range, are ignored on load and gone after the next save.
func TestOldBudgetFieldsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	b := newBinding(t, "")
	b.Autonomy, b.StopAgents = AutonomyFull, true
	data, err := json.Marshal(Settings{Version: Version, Node: "alice", Handler: worker.HandlerNone, Bindings: []ProjectBinding{b}}) //nolint:gosec // G117: a test file with made-up project secrets
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var old []map[string]any
	if err := json.Unmarshal(raw["project_bindings"], &old); err != nil || len(old) != 1 {
		t.Fatalf("bindings %s: %v", raw["project_bindings"], err)
	}
	old[0]["turns_per_hour"], old[0]["max_run_minutes"] = 100000, 1
	if raw["project_bindings"], err = json.Marshal(old); err != nil {
		t.Fatal(err)
	}
	if data, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := Load(path)
	if err != nil {
		t.Fatalf("an old file with budgets does not load: %v", err)
	}
	if got := s.Bindings[0]; got.AutonomyOf() != AutonomyFull || !got.StopAgents {
		t.Fatalf("binding %+v", got)
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Clean(path)); bytes.Contains(got, []byte("turns_per_hour")) || bytes.Contains(got, []byte("max_run_minutes")) {
		t.Fatalf("saved = %s", got)
	}
}
