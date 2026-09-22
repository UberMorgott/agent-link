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
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// DefaultAPI is the loopback address of the local web UI and control API.
const DefaultAPI = "127.0.0.1:7520"

// Settings is what the settings page edits. Only Node is required: without a
// code the node does not start, without Peers it runs, looks for members on
// the local networks and waits to be dialed.
type Settings struct {
	Node string `json:"node"`
	Code string `json:"code,omitempty"` // 6-character pairing code, same for every member
	// Peers are the addresses added by hand (IP, port optional); names are
	// learned from the handshake unless set. Members learned from them or by
	// discovery live in the node's own table, not here.
	Peers     []config.Peer `json:"peers,omitempty"`
	Handler   string        `json:"handler"` // worker.HandlerNone|HandlerClaude|HandlerCodex
	WorkDir   string        `json:"work_dir,omitempty"`
	Autostart bool          `json:"autostart"`

	// PeerAddr and PeerName are the single peer of v0.5 and earlier. Load and
	// Normalize move them into Peers; a posted peer_addr adds one address.
	PeerAddr string `json:"peer_addr,omitempty"`
	PeerName string `json:"peer_name,omitempty"`

	// "Дополнительно" on the page; all optional.
	Listen string   `json:"listen,omitempty"` // empty: every interface, port 7420
	API    string   `json:"api,omitempty"`    // empty: DefaultAPI; applies on the next start
	Areas  []string `json:"areas,omitempty"`
	// Projects maps an area name to the project a request addressed to that
	// area is handled in. Without an entry a request runs read-only in
	// WorkDir. Edited in the config file only.
	Projects map[string]Project `json:"projects,omitempty"`
	// Discovery looks for members on the local networks (UDP beacons); nil means on.
	Discovery *bool `json:"discovery,omitempty"`
	// MaxJobs is how many requests the agent answers at once, 1..worker.MaxMaxJobs;
	// 0 means worker.DefaultMaxJobs.
	MaxJobs int `json:"max_jobs,omitempty"`
	// AutoUpdate installs a newer GitHub release by itself; nil means on.
	// It has its own switch and is kept when the form is saved.
	AutoUpdate *bool `json:"auto_update,omitempty"`

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

// Project is one project directory requests of an area are handled in. With
// Write the agent runs write-capable there (it may edit files and run
// commands); without it the run stays read-only.
type Project struct {
	Dir   string `json:"dir"`
	Write bool   `json:"write,omitempty"`
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
	data, err := os.ReadFile(filepath.Clean(path))
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
	return s.migrated(), true, nil
}

// migrated moves the single legacy peer (PeerAddr, PeerName) into Peers.
// An address that does not parse stays in PeerAddr for Validate to report.
func (s Settings) migrated() Settings {
	s.PeerAddr, s.PeerName = strings.TrimSpace(s.PeerAddr), strings.TrimSpace(s.PeerName)
	if s.PeerAddr == "" {
		s.PeerName = ""
		return s
	}
	a, err := config.WithDefaultPort(s.PeerAddr)
	if err != nil {
		return s
	}
	s.Peers = slices.Clone(s.Peers)
	for i, p := range s.Peers {
		if p.Addr == a {
			if s.PeerName != "" {
				s.Peers[i].Name = s.PeerName
			}
			s.PeerAddr, s.PeerName = "", ""
			return s
		}
	}
	s.Peers = append(s.Peers, config.Peer{Name: s.PeerName, Addr: a})
	s.PeerAddr, s.PeerName = "", ""
	return s
}

// WithPeer returns s with addr (default port added) in Peers, once.
func (s Settings) WithPeer(addr string) Settings {
	s.PeerAddr, s.PeerName = addr, ""
	return s.migrated()
}

// DiscoveryOn reports whether members are looked for on the local networks.
func (s Settings) DiscoveryOn() bool { return s.Discovery == nil || *s.Discovery }

// Normalize trims the fields, upper-cases the code, adds the default port to
// the peer addresses and moves a legacy peer_addr into Peers. Invalid values
// are left for Validate to report.
func (s Settings) Normalize() Settings {
	s.Node, s.Listen = strings.TrimSpace(s.Node), strings.TrimSpace(s.Listen)
	s.API, s.WorkDir = strings.TrimSpace(s.API), strings.TrimSpace(s.WorkDir)
	s.Code, s.AgentPath = strings.TrimSpace(s.Code), strings.TrimSpace(s.AgentPath)
	if c, ok := config.NormalizeCode(s.Code); ok {
		s.Code = c
	}
	peers := make([]config.Peer, 0, len(s.Peers))
	for _, p := range s.Peers {
		p.Name, p.Addr = strings.TrimSpace(p.Name), strings.TrimSpace(p.Addr)
		if a, err := config.WithDefaultPort(p.Addr); err == nil {
			p.Addr = a
		}
		if !slices.ContainsFunc(peers, func(q config.Peer) bool { return q.Addr == p.Addr }) {
			peers = append(peers, p)
		}
	}
	s.Peers = nil
	if len(peers) > 0 {
		s.Peers = peers
	}
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
	}
	return s.migrated()
}

// Save validates and writes settings atomically with owner-only permissions.
func Save(path string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ") //nolint:gosec // G117: this file is where the secret is kept by design, written owner-only below
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

// AutoUpdateOn reports whether a newer release is installed by itself.
func (s Settings) AutoUpdateOn() bool { return s.AutoUpdate == nil || *s.AutoUpdate }

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
		Discovery: s.DiscoveryOn(),
	}
	for _, p := range s.migrated().Peers {
		if a, err := config.WithDefaultPort(p.Addr); err == nil {
			p.Addr = a
		}
		c.Peers = append(c.Peers, p)
	}
	return c
}

// Project returns the project directory requests of an area are handled in
// and whether the agent may write there. An empty dir means no project: the
// request runs read-only in WorkDir.
func (s Settings) Project(area string) (dir string, write bool) {
	p, ok := s.Projects[area]
	if !ok {
		return "", false
	}
	return p.Dir, p.Write
}

// Command returns the read-only agent command for the handler, or ok=false
// for none.
func (s Settings) Command() (worker.Command, bool) { return s.command(false) }

// WriteCommand returns the write-capable agent command for the handler, used
// for areas mapped to a project with write.
func (s Settings) WriteCommand() (worker.Command, bool) { return s.command(true) }

func (s Settings) command(write bool) (worker.Command, bool) {
	if s.Handler == worker.HandlerNone || s.Handler == "" {
		return worker.Command{}, false
	}
	c, ok := worker.ForHandler(s.Handler)
	if write {
		c, ok = worker.ForHandlerWrite(s.Handler)
	}
	if len(s.HandlerCommand) > 0 {
		// A stand-in for the handler's CLI: same output format, no preamble.
		return worker.Command{Name: s.HandlerCommand[0], Args: s.HandlerCommand[1:], Format: c.Format}, true
	}
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
	for _, p := range s.Peers {
		if _, err := config.WithDefaultPort(p.Addr); err != nil {
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
	if s.MaxJobs < 0 || s.MaxJobs > worker.MaxMaxJobs {
		return problem("max_jobs")
	}
	for _, a := range s.Areas {
		if !config.ValidName(a) {
			return problem("areas")
		}
	}
	for area, p := range s.Projects {
		if !config.ValidName(area) || p.Dir == "" || !filepath.IsAbs(p.Dir) {
			return problem("projects")
		}
	}
	seen := map[string]bool{}
	for _, name := range append([]string{s.PeerName}, peerNames(s.Peers)...) {
		switch {
		case name == "":
		case !config.ValidName(name):
			return problem("peer_name")
		case name == s.Node:
			return problem("peer_name_self")
		case seen[name]:
			return problem("peer_name_twice")
		}
		seen[name] = true
	}
	return nil
}

func peerNames(ps []config.Peer) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}
