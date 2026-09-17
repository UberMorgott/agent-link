// Package settings holds the desktop app's per-user configuration, stored
// with the shared secret in %APPDATA%\agentlink\config.json.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// DefaultAPI is the loopback address of the local web UI and control API.
const DefaultAPI = "127.0.0.1:7520"

// Settings is what the settings page edits.
type Settings struct {
	Node      string   `json:"node"`
	Listen    string   `json:"listen"`
	PeerName  string   `json:"peer_name"`
	PeerAddr  string   `json:"peer_addr"`
	Secret    string   `json:"secret"`
	Areas     []string `json:"areas"`
	Handler   string   `json:"handler"` // worker.HandlerNone|HandlerClaude|HandlerCodex
	WorkDir   string   `json:"work_dir"`
	Autostart bool     `json:"autostart"`

	// Not shown in the UI. API overrides DefaultAPI; HandlerCommand replaces
	// the built-in agent command (argv, used by tests with a fake agent).
	API            string   `json:"api,omitempty"`
	HandlerCommand []string `json:"handler_command,omitempty"`
}

// DefaultPath returns %APPDATA%\agentlink\config.json (the user config dir elsewhere).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agentlink", "config.json"), nil
}

// Load reads settings. A missing file returns ok=false and no error.
func Load(path string) (s Settings, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{Handler: worker.HandlerNone}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, true, nil
}

// Save validates and writes settings atomically with owner-only permissions.
func Save(path string, s Settings) error {
	if err := s.Validate(path); err != nil {
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

// NodeConfig converts settings stored at path into a node configuration. The
// node's data lives in a "data" directory next to the settings file.
func (s Settings) NodeConfig(path string) config.Config {
	c := config.Config{
		Node:      s.Node,
		Listen:    s.Listen,
		API:       s.APIAddr(),
		DataDir:   filepath.Join(filepath.Dir(path), "data"),
		SecretEnv: "-", // the secret comes from the settings file, not the environment
		Areas:     s.Areas,
	}
	if s.PeerName != "" || s.PeerAddr != "" {
		c.Peers = []config.Peer{{Name: s.PeerName, Addr: s.PeerAddr}}
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
	return worker.ForHandler(s.Handler)
}

// Validate checks everything a node and the handler need.
func (s Settings) Validate(path string) error {
	var errs []error
	if err := s.NodeConfig(path).Validate(); err != nil {
		errs = append(errs, err)
	}
	if s.PeerName == "" || s.PeerAddr == "" {
		errs = append(errs, errors.New("peer name and peer address are required"))
	}
	if len(s.Secret) < config.MinSecretLen {
		errs = append(errs, fmt.Errorf("shared secret must be at least %d characters", config.MinSecretLen))
	}
	if strings.TrimSpace(s.Secret) != s.Secret {
		errs = append(errs, errors.New("shared secret must not start or end with spaces"))
	}
	switch s.Handler {
	case worker.HandlerNone:
	case worker.HandlerClaude, worker.HandlerCodex:
		if st, err := os.Stat(s.WorkDir); s.WorkDir == "" || err != nil || !st.IsDir() {
			errs = append(errs, fmt.Errorf("working folder %q must be an existing folder", s.WorkDir))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown handler %q", s.Handler))
	}
	return errors.Join(errs...)
}
