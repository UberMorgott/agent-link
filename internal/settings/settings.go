// Package settings holds the desktop app's per-user configuration, stored
// with the pairing code in %APPDATA%\agentlink\config.json.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode"
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
	// Version is the file format (Version); Load migrates older files and
	// refuses newer ones.
	Version int    `json:"version"`
	Node    string `json:"node"`
	Code    string `json:"code,omitempty"` // 6-character pairing code, same for every member
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
	// area is handled in. Without an entry a request runs in
	// WorkDir. Edited in «Проекты» on the settings page; Normalize declares
	// every project area in Areas.
	Projects map[string]Project `json:"projects,omitempty"`
	// Discovery looks for members on the local networks (UDP beacons); nil means on.
	Discovery *bool `json:"discovery,omitempty"`
	// MaxJobs is how many requests the agent answers at once, 1..worker.MaxMaxJobs;
	// 0 means worker.DefaultMaxJobs.
	MaxJobs int `json:"max_jobs,omitempty"`
	// AutoAnswer («Автоответ агентом, если сессия не открыта») lets the handler's
	// agent answer a request headless when no live agent session is registered
	// for its area; nil means off (the default, also for configs from before it).
	// A save without the field keeps it.
	AutoAnswer *bool `json:"auto_answer,omitempty"`
	// AutoUpdate installs a newer GitHub release by itself; nil means on.
	// It has its own switch and is kept when the form is saved.
	AutoUpdate *bool `json:"auto_update,omitempty"`

	// Secret is the long shared secret of configs written before pairing
	// codes; used only while Code is empty. Never sent to the page.
	Secret string `json:"secret,omitempty"`
	// AgentPath is the absolute path of the agent program (.exe, .cmd, .bat)
	// when it is not on PATH. It replaces only the command name: the handler's
	// arguments stay.
	AgentPath string `json:"agent_path,omitempty"`
	// HandlerCommand replaces the built-in agent command (argv, used by tests)
	// and wins over AgentPath.
	HandlerCommand []string `json:"handler_command,omitempty"`

	// Bindings are the projects this member takes part in, with their
	// secrets. Never sent to the page: the invite reveal is the only way out.
	Bindings []ProjectBinding `json:"project_bindings,omitempty"`
}

// ProjectBinding is this member's local record of one project: the invite's
// parts and its own choices. Nothing of it is sent to other members.
type ProjectBinding struct {
	ID     string   `json:"id"`              // project id (config.ValidProjectID)
	Epoch  uint32   `json:"epoch"`           // config.ProjectEpoch in v1
	Secret string   `json:"secret"`          // canonical base32 of 16 random bytes
	Alias  string   `json:"alias,omitempty"` // this member's own name for it, ≤ maxAlias runes
	Dir    string   `json:"dir,omitempty"`   // bound folder; "" = none
	Peers  []string `json:"peers,omitempty"` // bootstrap addresses typed by the user (host:port)
	// AutoOpen opens a visible agent session in Dir when a message asks this
	// member and no session is live there (node.SetAutoOpen); nil means on.
	AutoOpen *bool `json:"auto_open,omitempty"`
	// LaunchMode is where such a session opens (node.SetLaunchMode):
	// "desktop" (the default, "") the agent's desktop app when installed,
	// "terminal" Windows Terminal.
	LaunchMode string `json:"launch_mode,omitempty"`
}

// LaunchModeOf is b's launch mode: "terminal" or "desktop".
func (b ProjectBinding) LaunchModeOf() string {
	if b.LaunchMode == "terminal" {
		return "terminal"
	}
	return "desktop"
}

// AutoOpenOn reports whether the project opens sessions by itself (default on).
func (b ProjectBinding) AutoOpenOn() bool { return b.AutoOpen == nil || *b.AutoOpen }

// Version is the settings file format this build writes. Version 2 added
// Bindings; a file without a version is version 1.
const Version = 2

// MaxProjects caps the bindings.
const MaxProjects = 32

// maxAlias caps a binding's Alias, in runes.
const maxAlias = 64

// ErrNewerVersion: the settings file was written by a newer agentlink. It is
// neither loaded nor overwritten.
var ErrNewerVersion = errors.New("settings from a newer agentlink")

// backupName is the copy of a version 1 file the migration writes once.
const backupName = "config.v1.bak.json"

// Project is one project directory requests of an area are handled in. Every
// request runs with full capability; an old "write" key in a saved file is
// ignored.
type Project struct {
	Dir string `json:"dir"`
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
	switch {
	case s.Version > Version:
		return Settings{}, false, fmt.Errorf("%s: %w (version %d)", path, ErrNewerVersion, s.Version)
	case s.Version < Version:
		// Version 1 → 2: keep a copy of the old file once, then the same
		// fields with the new version (no bindings yet).
		if err := writeBackup(filepath.Join(filepath.Dir(path), backupName), data); err != nil {
			return Settings{}, false, err
		}
		s.Version = Version
		if err := writeFile(path, s); err != nil {
			return Settings{}, false, err
		}
	}
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
	}
	return s.migrated(), true, nil
}

// writeBackup writes data to path unless a file is there already.
func writeBackup(path string, data []byte) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
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
	s.Projects, s.Areas = normalizeProjects(s.Projects, s.Areas)
	if len(s.Bindings) > 0 {
		bs := make([]ProjectBinding, len(s.Bindings))
		for i, b := range s.Bindings {
			b.Alias, b.Dir = strings.TrimSpace(b.Alias), strings.TrimSpace(b.Dir)
			if b.Dir != "" {
				b.Dir = filepath.Clean(b.Dir)
			}
			bs[i] = b
		}
		s.Bindings = bs
	}
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
	}
	return s.migrated()
}

// normalizeProjects trims the project areas and folders and declares every
// project area in areas: peers send an area message only to members that
// announced the area, so a project whose area is not announced gets nothing.
func normalizeProjects(projects map[string]Project, areas []string) (map[string]Project, []string) {
	if len(projects) == 0 {
		return nil, areas
	}
	out := make(map[string]Project, len(projects))
	for area, p := range projects {
		p.Dir = strings.TrimSpace(p.Dir)
		out[strings.TrimSpace(area)] = p
	}
	keys := slices.Sorted(maps.Keys(out))
	areas = slices.Clone(areas)
	for _, area := range keys {
		// An invalid area is left for Validate to report as a project problem.
		if config.ValidName(area) && !slices.Contains(areas, area) {
			areas = append(areas, area)
		}
	}
	return out, areas
}

// Save validates and writes settings atomically with owner-only permissions.
func Save(path string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	s.Version = Version
	return writeFile(path, s)
}

// writeFile writes s atomically, owner-only, unless the file there is from a
// newer agentlink.
func writeFile(path string, s Settings) error {
	if old, err := os.ReadFile(filepath.Clean(path)); err == nil {
		var v struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(old, &v) == nil && v.Version > Version {
			return fmt.Errorf("%s: %w (version %d)", path, ErrNewerVersion, v.Version)
		}
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

// AutoAnswerOn reports whether the handler answers requests no live session takes.
func (s Settings) AutoAnswerOn() bool { return s.AutoAnswer != nil && *s.AutoAnswer }

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

// Project returns the project directory requests of an area are handled in.
// An empty dir means no project: the request runs in WorkDir.
func (s Settings) Project(area string) string {
	return s.Projects[area].Dir
}

// Command returns the agent command for the handler, or ok=false for none.
func (s Settings) Command() (worker.Command, bool) {
	if s.Handler == worker.HandlerNone || s.Handler == "" {
		return worker.Command{}, false
	}
	c, ok := worker.ForHandler(s.Handler)
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
		// The working folder serves the legacy network only; projects bring
		// their own folders.
		if st, err := os.Stat(s.WorkDir); s.Key() != nil && (s.WorkDir == "" || err != nil || !st.IsDir()) {
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
	return validateBindings(s.Bindings)
}

// validateBindings checks every binding and that no two share an id or a
// folder (one folder may still hold another: the deepest one wins).
func validateBindings(bs []ProjectBinding) error {
	if len(bs) > MaxProjects {
		return problem("too_many_projects")
	}
	ids, dirs := map[string]bool{}, map[string]bool{}
	for _, b := range bs {
		switch {
		case !config.ValidProjectID(b.ID) || b.Epoch != config.ProjectEpoch || !config.ValidProjectSecret(b.Secret) || ids[b.ID]:
			return problem("project_binding")
		case !ValidAlias(b.Alias):
			return problem("alias")
		}
		ids[b.ID] = true
		for _, p := range b.Peers {
			if _, err := config.WithDefaultPort(p); err != nil {
				return problem("addr")
			}
		}
		if b.Dir == "" {
			continue
		}
		if st, err := os.Stat(b.Dir); !filepath.IsAbs(b.Dir) || err != nil || !st.IsDir() {
			return problem("dir")
		}
		key := DirKey(b.Dir)
		if dirs[key] {
			return problem("dir_taken")
		}
		dirs[key] = true
	}
	return nil
}

// ValidAlias reports a local project alias: at most 64 runes after trimming,
// no control characters ("" = none).
func ValidAlias(s string) bool {
	s = strings.TrimSpace(s)
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= maxAlias && !strings.ContainsFunc(s, unicode.IsControl)
}

// DirKey is the form of a folder path two bindings are compared by: cleaned,
// and case-folded on Windows.
func DirKey(dir string) string {
	dir = filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		dir = strings.ToLower(dir)
	}
	return dir
}

func peerNames(ps []config.Peer) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}
