package settings

import (
	"bytes"
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
		Node: "alice", Listen: "10.147.20.5:7420", PeerName: "bob", PeerAddr: "10.147.20.9:7420",
		Code: "ABC123", Areas: []string{"dev"},
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
		"short code":     func(s *Settings) { s.Code = "ABC12" },
		"long code":      func(s *Settings) { s.Code = "ABC1234" },
		"code charset":   func(s *Settings) { s.Code = "ABC-12" },
		"cyrillic code":  func(s *Settings) { s.Code = "АВС123" },
		"no name":        func(s *Settings) { s.Node = "" },
		"same name":      func(s *Settings) { s.PeerName = s.Node },
		"bad listen":     func(s *Settings) { s.Listen = "no pe" },
		"all interfaces": func(s *Settings) { s.Listen = "0.0.0.0" },
		"bad peer port":  func(s *Settings) { s.PeerAddr = "10.0.0.1:70000" },
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
	if s.Node != "morgott" || s.Code != "K7Q2MX" || s.PeerAddr != "10.147.20.9:7420" || s.Handler != worker.HandlerNone {
		t.Fatalf("normalized %+v", s)
	}
	if s := (Settings{PeerAddr: "fd00::1"}).Normalize(); s.PeerAddr != "[fd00::1]:7420" {
		t.Fatalf("ipv6 peer %q", s.PeerAddr)
	}
	if s := (Settings{PeerAddr: "10.0.0.2:7500"}).Normalize(); s.PeerAddr != "10.0.0.2:7500" {
		t.Fatalf("explicit port changed: %q", s.PeerAddr)
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
		{"adapter down", []Iface{eth, zt(false, "10.147.20.5")}, "127.0.0.1:7420", false},
		{"link-local only", []Iface{zt(true, "fe80::1", "169.254.3.4")}, "127.0.0.1:7420", false},
		{"no zerotier", []Iface{eth, lo}, "127.0.0.1:7420", false},
		{"no interfaces", nil, "127.0.0.1:7420", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Settings{}.ListenAddr(c.ifaces)
			if got != c.want || ok != c.zeroTier {
				t.Fatalf("ListenAddr = %q, %v; want %q, %v", got, ok, c.want, c.zeroTier)
			}
		})
	}
	// An explicit listen wins and never falls back.
	if got, ok := (Settings{Listen: "10.9.9.9"}).ListenAddr(nil); got != "10.9.9.9:7420" || !ok {
		t.Fatalf("explicit listen = %q %v", got, ok)
	}
}
