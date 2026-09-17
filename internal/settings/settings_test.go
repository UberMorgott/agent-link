package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/worker"
)

func valid(t *testing.T) Settings {
	return Settings{
		Node: "alice", Listen: "10.147.20.5:7420", PeerName: "bob", PeerAddr: "10.147.20.9:7420",
		Secret: strings.Repeat("s", 40), Areas: []string{"dev"},
		Handler: worker.HandlerClaude, WorkDir: t.TempDir(),
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentlink", "config.json")
	if _, ok, err := Load(path); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}
	want := valid(t)
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(path)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	cfg := got.NodeConfig(path)
	if cfg.API != DefaultAPI || cfg.DataDir != filepath.Join(filepath.Dir(path), "data") || len(cfg.Peers) != 1 {
		t.Fatalf("node config %+v", cfg)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cases := map[string]func(*Settings){
		"short secret":   func(s *Settings) { s.Secret = "short" },
		"no peer":        func(s *Settings) { s.PeerAddr = "" },
		"same name":      func(s *Settings) { s.PeerName = s.Node },
		"bad listen":     func(s *Settings) { s.Listen = "nope" },
		"missing folder": func(s *Settings) { s.WorkDir = filepath.Join(s.WorkDir, "absent") },
		"bad handler":    func(s *Settings) { s.Handler = "vim" },
		"public api":     func(s *Settings) { s.API = "0.0.0.0:7520" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := valid(t)
			mutate(&s)
			if err := Save(path, s); err == nil {
				t.Fatal("expected an error")
			}
			if _, err := os.Stat(path); err == nil {
				t.Fatal("invalid settings were written")
			}
		})
	}
}

func TestCommand(t *testing.T) {
	s := valid(t)
	if c, ok := s.Command(); !ok || c.Name != "claude" {
		t.Fatalf("claude: %+v %v", c, ok)
	}
	s.HandlerCommand = []string{"fake.exe", "--echo"}
	if c, ok := s.Command(); !ok || c.Name != "fake.exe" || len(c.Args) != 1 {
		t.Fatalf("override: %+v %v", c, ok)
	}
	s.Handler = worker.HandlerNone
	if _, ok := s.Command(); ok {
		t.Fatal("none must have no command")
	}
}
