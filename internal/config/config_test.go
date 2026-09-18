package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Node: "a", Listen: "10.147.0.1:7420", API: "127.0.0.1:7520",
		DataDir: "data", SecretEnv: "AGENTLINK_SECRET",
		Peers: []Peer{{Name: "b", Addr: "10.147.0.2:7420"}},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		errSub string
	}{
		{"valid", func(*Config) {}, ""},
		{"api on all interfaces", func(c *Config) { c.API = "0.0.0.0:7520" }, "loopback"},
		{"api on lan ip", func(c *Config) { c.API = "10.147.0.1:7520" }, "loopback"},
		{"peer named like self", func(c *Config) { c.Peers[0].Name = "a" }, "this node's name"},
		{"missing secret env", func(c *Config) { c.SecretEnv = "" }, "secret_env"},
		{"bad node name", func(c *Config) { c.Node = "../x" }, "invalid node name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			if tt.errSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errSub) {
				t.Fatalf("error %v, want substring %q", err, tt.errSub)
			}
		})
	}
}

func TestLoadResolvesDataDirAndReadsSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.json")
	data := `{"node":"a","listen":"127.0.0.1:7421","api":"127.0.0.1:7521","data_dir":"d","secret_env":"AGENTLINK_TEST_SECRET"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != filepath.Join(dir, "d") {
		t.Fatalf("data_dir = %q", c.DataDir)
	}
	t.Setenv("AGENTLINK_TEST_SECRET", "short")
	if _, err := c.Secret(); err == nil {
		t.Fatal("short secret accepted")
	}
	t.Setenv("AGENTLINK_TEST_SECRET", "0123456789abcdef0123")
	if _, err := c.Secret(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{"abc123": "ABC123", " K7q2Mx ": "K7Q2MX", "ZZZZZZ": "ZZZZZZ"} {
		if got, ok := NormalizeCode(in); !ok || got != want {
			t.Errorf("NormalizeCode(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "ABC12", "ABC1234", "ABC 12", "ABC-12", "АВС123", "abc12é"} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("NormalizeCode(%q) accepted", bad)
		}
	}
	a, err := KeyFromCode("k7q2mx")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := KeyFromCode("K7Q2MX")
	if len(a) != 32 || string(a) != string(b) {
		t.Fatal("key depends on case")
	}
}

func TestSecretAcceptsCode(t *testing.T) {
	c := validConfig()
	t.Setenv("AGENTLINK_SECRET", "k7q2mx")
	k, err := c.Secret()
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := KeyFromCode("K7Q2MX"); string(k) != string(want) {
		t.Fatal("env code not derived like the tray's code")
	}
}

func TestWithDefaultPort(t *testing.T) {
	for in, want := range map[string]string{
		"10.147.20.9": "10.147.20.9:7420", "10.147.20.9:7500": "10.147.20.9:7500",
		"fd00::1": "[fd00::1]:7420", "[fd00::1]": "[fd00::1]:7420", "[fd00::1]:9": "[fd00::1]:9",
	} {
		if got, err := WithDefaultPort(in); err != nil || got != want {
			t.Errorf("WithDefaultPort(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "10.0.0.1:x", "10.0.0.1:70000", "a b"} {
		if _, err := WithDefaultPort(bad); err == nil {
			t.Errorf("WithDefaultPort(%q) accepted", bad)
		}
	}
}

func TestNamelessPeerValid(t *testing.T) {
	c := validConfig()
	c.Peers = []Peer{{Addr: "10.147.0.2:7420"}}
	c.Node = "Никита"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
