package settings

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/worker"
)

func valid(t *testing.T) Settings {
	return Settings{
		Node: "alice", Listen: "10.147.20.5:7420", Peers: []config.Peer{{Name: "bob", Addr: "10.147.20.9:7420"}},
		Code: "ABC123", Areas: []string{"dev"},
		Handler: worker.HandlerClaude, WorkDir: t.TempDir(),
	}
}

func TestValidateRejectsHandlerNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := valid(t)
	s.Handler = worker.HandlerCodex
	var p *Problem
	if err := s.Validate(); !errors.As(err, &p) || p.Key != "handler_missing" {
		t.Fatalf("Validate() = %v, want handler_missing", err)
	}
	s.HandlerCommand = []string{"fake"}
	if err := s.Validate(); err != nil {
		t.Fatalf("custom command must skip the PATH check: %v", err)
	}
}

// fakeClaudeOnPath makes Validate find a claude executable without the real
// CLI installed (CI runners have none).
func fakeClaudeOnPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"claude", "claude.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	fakeClaudeOnPath(t)
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
	cfg := got.NodeConfig(path, got.Listen)
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
		"short code":       func(s *Settings) { s.Code = "ABC12" },
		"long code":        func(s *Settings) { s.Code = "ABC1234" },
		"code charset":     func(s *Settings) { s.Code = "ABC-12" },
		"cyrillic code":    func(s *Settings) { s.Code = "АВС123" },
		"no name":          func(s *Settings) { s.Node = "" },
		"same name":        func(s *Settings) { s.Peers[0].Name = s.Node },
		"legacy same":      func(s *Settings) { s.PeerName = s.Node },
		"bad peer in list": func(s *Settings) { s.Peers[0].Addr = "10.0.0.1:70000" },
		"bad listen":       func(s *Settings) { s.Listen = "no pe" },
		"all interfaces":   func(s *Settings) { s.Listen = "0.0.0.0" },
		"bad peer port":    func(s *Settings) { s.PeerAddr = "10.0.0.1:70000" },
		"missing folder":   func(s *Settings) { s.WorkDir = filepath.Join(s.WorkDir, "absent") },
		"bad handler":      func(s *Settings) { s.Handler = "vim" },
		"public api":       func(s *Settings) { s.API = "0.0.0.0:7520" },
		"max jobs high":    func(s *Settings) { s.MaxJobs = 5 },
		"max jobs neg":     func(s *Settings) { s.MaxJobs = -1 },
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

// A first save may carry only a name: no code, no peer, defaults elsewhere.
func TestPartialSettingsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Settings{Node: "morgott", Handler: worker.HandlerNone}); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Load(path)
	if err != nil || !ok || s.Key() != nil || len(s.NodeConfig(path, "127.0.0.1:7420").Peers) != 0 {
		t.Fatalf("partial settings %+v ok=%v err=%v", s, ok, err)
	}
	if d, _, _ := Load(filepath.Join(t.TempDir(), "absent.json")); !config.ValidName(d.Node) {
		t.Fatalf("default name %q is not a valid node name", d.Node)
	}
}

func TestNormalize(t *testing.T) {
	s := Settings{Node: " morgott ", Code: " k7q2mx ", PeerAddr: "10.147.20.9"}.Normalize()
	if s.Node != "morgott" || s.Code != "K7Q2MX" || len(s.Peers) != 1 || s.Peers[0].Addr != "10.147.20.9:7420" || s.Handler != worker.HandlerNone {
		t.Fatalf("normalized %+v", s)
	}
	if s := (Settings{PeerAddr: "fd00::1"}).Normalize(); s.Peers[0].Addr != "[fd00::1]:7420" {
		t.Fatalf("ipv6 peer %+v", s.Peers)
	}
	if s := (Settings{Peers: []config.Peer{{Addr: " 10.0.0.2:7500 "}}}).Normalize(); s.Peers[0].Addr != "10.0.0.2:7500" {
		t.Fatalf("explicit port changed: %+v", s.Peers)
	}
}

// The same code typed in different case on two machines gives the same key;
// a legacy secret is used as is while no code is set.
func TestKey(t *testing.T) {
	a, b := Settings{Code: "k7q2mx"}.Key(), Settings{Code: "K7Q2MX"}.Key()
	if len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatalf("keys differ by case: %x %x", a, b)
	}
	if c := (Settings{Code: "K7Q2MY"}).Key(); bytes.Equal(a, c) {
		t.Fatal("different codes gave the same key")
	}
	legacy := strings.Repeat("s", 40)
	if k := (Settings{Secret: legacy}).Key(); string(k) != legacy {
		t.Fatal("legacy secret not used")
	}
	if k := (Settings{Secret: legacy, Code: "ABC123"}).Key(); string(k) == legacy {
		t.Fatal("code must win over the legacy secret")
	}
	if (Settings{}).Key() != nil || (Settings{Secret: "short"}).Key() != nil {
		t.Fatal("key without a code or long secret")
	}
}

func TestZeroTierDetection(t *testing.T) {
	zt := func(up bool, ips ...string) Iface {
		it := Iface{Name: "ZeroTier One [8056c2e21c000001]", Up: up}
		for _, s := range ips {
			it.Addrs = append(it.Addrs, net.ParseIP(s))
		}
		return it
	}
	eth := Iface{Name: "Ethernet", Up: true, Addrs: []net.IP{net.ParseIP("192.168.1.10")}}
	lo := Iface{Name: "Loopback Pseudo-Interface 1", Up: true, Addrs: []net.IP{net.ParseIP("127.0.0.1")}}
	cases := []struct {
		name     string
		ifaces   []Iface
		want     string
		zeroTier bool
	}{
		{"zerotier ipv4", []Iface{eth, zt(true, "fe80::1", "10.147.20.5"), lo}, "10.147.20.5:7420", true},
		{"lower-case name", []Iface{{Name: "zerotier0", Up: true, Addrs: []net.IP{net.ParseIP("10.1.2.3")}}}, "10.1.2.3:7420", true},
		{"ipv6 only", []Iface{zt(true, "fe80::1", "fd80:56c2::5")}, "[fd80:56c2::5]:7420", true},
		{"adapter down", []Iface{eth, zt(false, "10.147.20.5")}, "192.168.1.10:7420", false},
		{"link-local only", []Iface{zt(true, "fe80::1", "169.254.3.4")}, "127.0.0.1:7420", false},
		{"no zerotier", []Iface{lo, eth}, "192.168.1.10:7420", false},
		{"no interfaces", nil, "127.0.0.1:7420", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Settings{}.AdvertiseAddr(c.ifaces)
			if got != c.want || ok != c.zeroTier {
				t.Fatalf("AdvertiseAddr = %q, %v; want %q, %v", got, ok, c.want, c.zeroTier)
			}
		})
	}
	// An explicit listen wins and never falls back; empty binds every interface.
	if got, ok := (Settings{Listen: "10.9.9.9"}).AdvertiseAddr(nil); got != "10.9.9.9:7420" || !ok {
		t.Fatalf("explicit listen = %q %v", got, ok)
	}
	if b := (Settings{Listen: "10.9.9.9"}).BindAddr(); b != "10.9.9.9:7420" {
		t.Fatalf("explicit bind %q", b)
	}
	if b := (Settings{}).BindAddr(); b != ":7420" {
		t.Fatalf("default bind %q", b)
	}
}

// A config with the single peer_addr/peer_name of v0.5 and earlier loads as a
// one-entry peer list, and a posted peer_addr is added to the list.
func TestPeerMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"node":"alice","code":"ABC123","peer_addr":"10.147.20.9","peer_name":"bob","handler":"none"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Load(path)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := []config.Peer{{Name: "bob", Addr: "10.147.20.9:7420"}}
	if !reflect.DeepEqual(s.Peers, want) || s.PeerAddr != "" || s.PeerName != "" {
		t.Fatalf("migrated %+v", s)
	}
	if cfg := s.NodeConfig(path, ":7420"); !reflect.DeepEqual(cfg.Peers, want) || !cfg.Discovery {
		t.Fatalf("node config %+v", cfg)
	}
	s.PeerAddr = "10.147.20.10"
	s = s.Normalize()
	if len(s.Peers) != 2 || s.Peers[1].Addr != "10.147.20.10:7420" || s.PeerAddr != "" {
		t.Fatalf("posted peer_addr: %+v", s.Peers)
	}
	if s = s.WithPeer("10.147.20.10:7420"); len(s.Peers) != 2 {
		t.Fatalf("duplicate added: %+v", s.Peers)
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Clean(path))
	if strings.Contains(string(data), "peer_addr") || !strings.Contains(string(data), `"peers"`) {
		t.Fatalf("saved %s", data)
	}
	off := false
	if cfg := (Settings{Node: "a", Discovery: &off}).NodeConfig(path, ":7420"); cfg.Discovery {
		t.Fatal("discovery switched off still on")
	}
}
