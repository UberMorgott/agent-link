package settings

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/worker"
)

func newBinding(t *testing.T, dir string) ProjectBinding {
	t.Helper()
	id, err := config.NewProjectID()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := config.NewProjectSecret()
	if err != nil {
		t.Fatal(err)
	}
	return ProjectBinding{ID: id, Epoch: config.ProjectEpoch, Secret: secret, Dir: dir}
}

func TestMigrationV1WritesBackupOnce(t *testing.T) {
	dir := t.TempDir()
	path, backup := filepath.Join(dir, "config.json"), filepath.Join(dir, backupName)
	v1 := []byte(`{"node":"alice","code":"K7Q2-MXPA-4RTB","handler":"none","secret":"kept-secret-0123456789","areas":["dev"]}`)
	if err := os.WriteFile(path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Load(path)
	if err != nil || !ok || s.Version != Version || s.Node != "alice" || s.Code != "K7Q2-MXPA-4RTB" || s.Secret != "kept-secret-0123456789" || len(s.Areas) != 1 {
		t.Fatalf("Load = %+v, %v, %v", s, ok, err)
	}
	if got, _ := os.ReadFile(filepath.Clean(backup)); !bytes.Equal(got, v1) {
		t.Fatalf("backup = %s", got)
	}
	if got, _ := os.ReadFile(filepath.Clean(path)); !bytes.Contains(got, []byte(`"version": 2`)) || !bytes.Contains(got, []byte("kept-secret")) {
		t.Fatalf("migrated file = %s", got)
	}
	// A later v1 file (a downgrade wrote it) never replaces the first backup.
	if err := os.WriteFile(path, []byte(`{"node":"bob","handler":"none"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _, err := Load(path); err != nil || s.Node != "bob" {
		t.Fatalf("second migration: %+v, %v", s, err)
	}
	if got, _ := os.ReadFile(filepath.Clean(backup)); !bytes.Equal(got, v1) {
		t.Fatalf("backup overwritten: %s", got)
	}
	// Version 2 loads as is.
	before, _ := os.ReadFile(filepath.Clean(path))
	if _, _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Clean(path)); !bytes.Equal(before, after) {
		t.Fatal("a v2 file was rewritten on load")
	}
}

func TestNewerVersionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	newer := []byte(`{"version":3,"node":"alice","handler":"none"}`)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("Load = %v, want ErrNewerVersion", err)
	}
	if err := Save(path, Settings{Node: "alice", Handler: worker.HandlerNone}); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("Save over a newer file = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Clean(path)); !bytes.Equal(got, newer) {
		t.Fatalf("newer file overwritten: %s", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), backupName)); err == nil {
		t.Fatal("backup written for a newer file")
	}
}

func TestBindingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	b := newBinding(t, t.TempDir())
	b.Alias, b.Peers = "Сайт", []string{"10.147.20.9:7420"}
	s := Settings{Node: "alice", Handler: worker.HandlerNone, Bindings: []ProjectBinding{b, newBinding(t, "")}}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(path)
	if err != nil || got.Version != Version || len(got.Bindings) != 2 || got.Bindings[0].Secret != b.Secret || got.Bindings[0].Alias != "Сайт" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestValidateBindings(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	nested := filepath.Join(dirA, "sub")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	ok := []ProjectBinding{newBinding(t, dirA), newBinding(t, nested), newBinding(t, ""), newBinding(t, "")}
	if err := (Settings{Node: "alice", Bindings: ok}).Normalize().Validate(); err != nil {
		t.Fatalf("nested and empty dirs rejected: %v", err)
	}
	dupDir := strings.ToUpper(dirA[:1]) + dirA[1:] + string(filepath.Separator)
	if filepath.Separator == '\\' {
		dupDir = strings.ToUpper(dirA)
	}
	many := make([]ProjectBinding, MaxProjects+1)
	for i := range many {
		many[i] = newBinding(t, "")
	}
	cases := map[string]struct {
		mutate func(bs []ProjectBinding) []ProjectBinding
		key    string
	}{
		"bad id":         {func(bs []ProjectBinding) []ProjectBinding { bs[0].ID = "legacy"; return bs }, "project_binding"},
		"lower id":       {func(bs []ProjectBinding) []ProjectBinding { bs[0].ID = strings.ToLower(bs[0].ID); return bs }, "project_binding"},
		"epoch 2":        {func(bs []ProjectBinding) []ProjectBinding { bs[0].Epoch = 2; return bs }, "project_binding"},
		"short secret":   {func(bs []ProjectBinding) []ProjectBinding { bs[0].Secret = bs[0].Secret[:25]; return bs }, "project_binding"},
		"duplicate id":   {func(bs []ProjectBinding) []ProjectBinding { bs[1].ID = bs[0].ID; return bs }, "project_binding"},
		"long alias":     {func(bs []ProjectBinding) []ProjectBinding { bs[0].Alias = strings.Repeat("я", 65); return bs }, "alias"},
		"control alias":  {func(bs []ProjectBinding) []ProjectBinding { bs[0].Alias = "a\x07b"; return bs }, "alias"},
		"relative dir":   {func(bs []ProjectBinding) []ProjectBinding { bs[0].Dir = "rel"; return bs }, "dir"},
		"missing dir":    {func(bs []ProjectBinding) []ProjectBinding { bs[0].Dir = filepath.Join(dirB, "absent"); return bs }, "dir"},
		"duplicate dir":  {func(bs []ProjectBinding) []ProjectBinding { bs[2].Dir = dirA; return bs }, "dir_taken"},
		"case-fold dir":  {func(bs []ProjectBinding) []ProjectBinding { bs[2].Dir = dupDir; return bs }, "dir_taken"},
		"bad peer":       {func(bs []ProjectBinding) []ProjectBinding { bs[0].Peers = []string{"10.0.0.1:70000"}; return bs }, "addr"},
		"too many":       {func([]ProjectBinding) []ProjectBinding { return many }, "too_many_projects"},
		"alias 64 runes": {func(bs []ProjectBinding) []ProjectBinding { bs[0].Alias = strings.Repeat("я", 64); return bs }, ""},
	}
	for name, c := range cases {
		bs := c.mutate(append([]ProjectBinding(nil), ok...))
		err := (Settings{Node: "alice", Bindings: bs}).Normalize().Validate()
		var p *Problem
		switch {
		case c.key == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.key != "" && (!errors.As(err, &p) || p.Key != c.key):
			t.Errorf("%s: Validate() = %v, want %s", name, err, c.key)
		}
	}
}

func TestHandlerWithoutLegacyKeyNeedsNoWorkDir(t *testing.T) {
	fakeClaudeOnPath(t)
	s := Settings{Node: "alice", Handler: worker.HandlerClaude}
	if err := s.Validate(); err != nil {
		t.Fatalf("handler without a legacy key: %v", err)
	}
	s.Code = "K7Q2-MXPA-4RTB"
	var p *Problem
	if err := s.Validate(); !errors.As(err, &p) || p.Key != "work_dir" {
		t.Fatalf("handler with a legacy key and no work dir: %v", err)
	}
}
