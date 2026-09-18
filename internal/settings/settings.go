// Package settings holds the desktop app's per-user configuration, stored
// with the pairing code in %APPDATA%\agentlink\config.json.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// DefaultAPI is the loopback address of the local web UI and control API.
const DefaultAPI = "127.0.0.1:7520"

// Settings is what the settings page edits. Only Node is required: without a
// code the node does not start, without PeerAddr it runs and waits to be dialed.
type Settings struct {
	Node      string `json:"node"`
	Code      string `json:"code,omitempty"`      // 6-character pairing code, same on both sides
	PeerAddr  string `json:"peer_addr,omitempty"` // the other side's ZeroTier IP, port optional
	Handler   string `json:"handler"`             // worker.HandlerNone|HandlerClaude|HandlerCodex
	WorkDir   string `json:"work_dir,omitempty"`
	Autostart bool   `json:"autostart"`

	// "Дополнительно" on the page; all optional.
	Listen   string   `json:"listen,omitempty"` // empty: the ZeroTier IP, else 127.0.0.1
	API      string   `json:"api,omitempty"`    // empty: DefaultAPI; applies on the next start
	Areas    []string `json:"areas,omitempty"`
	PeerName string   `json:"peer_name,omitempty"` // empty: learned from the handshake

	// Secret is the long shared secret of configs written before pairing
	// codes; used only while Code is empty. Never sent to the page.
	Secret string `json:"secret,omitempty"`
	// AgentPath is the absolute path of the agent program (.exe, .cmd, .bat)
	// when it is not on PATH. It replaces only the command name: the handler's
	// read-only arguments stay.
	AgentPath string `json:"agent_path,omitempty"`
	// HandlerCommand replaces the built-in agent command (argv, used by tests)
	// and wins over AgentPath.
	HandlerCommand []string `json:"handler_command,omitempty"`
}

// Problem is a validation failure. Key names the field or rule; the web UI
// turns it into one sentence of advice.
type Problem struct{ Key string }

func (p *Problem) Error() string { return "invalid settings: " + p.Key }

func problem(key string) error { return &Problem{Key: key} }

// DefaultPath returns %APPDATA%\agentlink\config.json (the user config dir elsewhere).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agentlink", "config.json"), nil
}

// Defaults are the settings of a fresh install: the Windows user (or computer)
// name and no handler.
func Defaults() Settings {
	return Settings{Node: DefaultName(), Handler: worker.HandlerNone}
}

var nameJunk = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// DefaultName derives a valid node name from the user or computer name.
func DefaultName() string {
	host, _ := os.Hostname()
	for _, raw := range []string{os.Getenv("USERNAME"), os.Getenv("USER"), host} {
		s := strings.Trim(nameJunk.ReplaceAllString(raw, "-"), "-_")
		for utf8.RuneCountInString(s) > 64 {
			_, size := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-size]
		}
		if config.ValidName(s) {
			return s
		}
	}
	return "me"
}

// Load reads settings. A missing file returns Defaults, ok=false and no error.
func Load(path string) (s Settings, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
	}
	return s, true, nil
}

// Normalize trims the fields, upper-cases the code and adds the default port
// to the peer address. Invalid values are left for Validate to report.
func (s Settings) Normalize() Settings {
	s.Node, s.PeerAddr, s.Listen = strings.TrimSpace(s.Node), strings.TrimSpace(s.PeerAddr), strings.TrimSpace(s.Listen)
	s.API, s.PeerName, s.WorkDir = strings.TrimSpace(s.API), strings.TrimSpace(s.PeerName), strings.TrimSpace(s.WorkDir)
	s.Code, s.AgentPath = strings.TrimSpace(s.Code), strings.TrimSpace(s.AgentPath)
	if c, ok := config.NormalizeCode(s.Code); ok {
		s.Code = c
	}
	if s.PeerAddr != "" {
		if a, err := config.WithDefaultPort(s.PeerAddr); err == nil {
			s.PeerAddr = a
		}
	}
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
	}
	return s
}

// Save validates and writes settings atomically with owner-only permissions.
func Save(path string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// APIAddr returns the effective control API address.
func (s Settings) APIAddr() string {
	if s.API != "" {
		return s.API
	}
	return DefaultAPI
}

// Key returns the session key: derived from Code, else the legacy Secret, else
// nil (the node cannot authenticate anyone and is not started).
func (s Settings) Key() []byte {
	if s.Code != "" {
		if k, err := config.KeyFromCode(s.Code); err == nil {
			return k
		}
		return nil
	}
	if len(s.Secret) >= config.MinSecretLen {
		return []byte(s.Secret)
	}
	return nil
}

// NodeConfig converts settings stored at path into a node configuration that
// listens on listen. The node's data lives in a "data" directory next to the
// settings file.
func (s Settings) NodeConfig(path, listen string) config.Config {
	c := config.Config{
		Node:      s.Node,
		Listen:    listen,
		API:       s.APIAddr(),
		DataDir:   filepath.Join(filepath.Dir(path), "data"),
		SecretEnv: "-", // the key comes from the settings file, not the environment
		Areas:     s.Areas,
	}
	if s.PeerAddr != "" {
		addr, err := config.WithDefaultPort(s.PeerAddr)
		if err != nil {
			addr = s.PeerAddr
		}
		c.Peers = []config.Peer{{Name: s.PeerName, Addr: addr}}
	}
	return c
}

// Command returns the agent command for the handler, or ok=false for none.
func (s Settings) Command() (worker.Command, bool) {
	if s.Handler == worker.HandlerNone || s.Handler == "" {
		return worker.Command{}, false
	}
	if len(s.HandlerCommand) > 0 {
		return worker.Command{Name: s.HandlerCommand[0], Args: s.HandlerCommand[1:]}, true
	}
	c, ok := worker.ForHandler(s.Handler)
	if ok && s.AgentPath != "" {
		c.Name = s.AgentPath
	}
	return c, ok
}

// Validate reports the first problem, in page order. Empty code and empty
// peer address are allowed: the node then waits for them.
func (s Settings) Validate() error {
	if !config.ValidName(s.Node) {
		return problem("node")
	}
	if s.Code != "" {
		if _, ok := config.NormalizeCode(s.Code); !ok {
			return problem("code")
		}
	}
	if s.PeerAddr != "" {
		if _, err := config.WithDefaultPort(s.PeerAddr); err != nil {
			return problem("peer_addr")
		}
	}
	switch s.Handler {
	case worker.HandlerNone, "":
	case worker.HandlerClaude, worker.HandlerCodex:
		if st, err := os.Stat(s.WorkDir); s.WorkDir == "" || err != nil || !st.IsDir() {
			return problem("work_dir")
		}
		switch {
		case len(s.HandlerCommand) > 0:
		case s.AgentPath != "":
			if st, err := os.Stat(s.AgentPath); !filepath.IsAbs(s.AgentPath) || err != nil || !st.Mode().IsRegular() {
				return problem("agent_path")
			}
		default:
			if _, err := exec.LookPath(s.Handler); err != nil {
				return problem("handler_missing")
			}
		}
	default:
		return problem("handler")
	}
	if s.Listen != "" {
		// Never all interfaces: the peer port belongs on the private network only.
		a, err := config.WithDefaultPort(s.Listen)
		if err != nil {
			return problem("listen_format")
		}
		if host, _, _ := net.SplitHostPort(a); net.ParseIP(host) == nil || net.ParseIP(host).IsUnspecified() {
			return problem("listen_format")
		}
	}
	if s.API != "" && !config.IsLoopbackAddr(s.API) {
		return problem("api")
	}
	for _, a := range s.Areas {
		if !config.ValidName(a) {
			return problem("areas")
		}
	}
	if s.PeerName != "" {
		if !config.ValidName(s.PeerName) {
			return problem("peer_name")
		}
		if s.PeerName == s.Node {
			return problem("peer_name_self")
		}
	}
	return nil
}
