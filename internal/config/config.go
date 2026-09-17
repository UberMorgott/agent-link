// Package config loads and validates an agentlink node configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
)

// MinSecretLen is the minimum accepted length of the shared secret in bytes.
const MinSecretLen = 16

// Peer is a remote node this node dials and accepts.
type Peer struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// Config is the on-disk node configuration. The shared secret is never stored
// here; SecretEnv names the environment variable that holds it.
type Config struct {
	Node      string   `json:"node"`
	Listen    string   `json:"listen"`
	API       string   `json:"api"`
	DataDir   string   `json:"data_dir"`
	SecretEnv string   `json:"secret_env"`
	Areas     []string `json:"areas"`
	Peers     []Peer   `json:"peers"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidName reports whether s is usable as a node or area name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// IsLoopbackAddr reports whether a host:port address binds to loopback only.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return IsLoopbackHost(host)
}

// IsLoopbackHost reports whether host is localhost or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Load reads a config file. A relative data_dir resolves against the config
// file's directory.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(path), c.DataDir)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Validate checks required fields and the loopback-only API rule.
func (c Config) Validate() error {
	var errs []error
	if !ValidName(c.Node) {
		errs = append(errs, fmt.Errorf("invalid node name %q", c.Node))
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("invalid listen %q: %w", c.Listen, err))
	}
	if !IsLoopbackAddr(c.API) {
		errs = append(errs, fmt.Errorf("api %q must be a loopback host:port", c.API))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir is required"))
	}
	if c.SecretEnv == "" {
		errs = append(errs, errors.New("secret_env is required"))
	}
	for _, a := range c.Areas {
		if !ValidName(a) {
			errs = append(errs, fmt.Errorf("invalid area %q", a))
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Peers {
		switch {
		case !ValidName(p.Name):
			errs = append(errs, fmt.Errorf("invalid peer name %q", p.Name))
		case p.Name == c.Node:
			errs = append(errs, fmt.Errorf("peer %q has this node's name", p.Name))
		case seen[p.Name]:
			errs = append(errs, fmt.Errorf("duplicate peer %q", p.Name))
		}
		seen[p.Name] = true
		if _, _, err := net.SplitHostPort(p.Addr); err != nil {
			errs = append(errs, fmt.Errorf("peer %q: invalid addr %q", p.Name, p.Addr))
		}
	}
	return errors.Join(errs...)
}

// Secret reads the shared secret from the environment variable named by SecretEnv.
func (c Config) Secret() ([]byte, error) {
	s := os.Getenv(c.SecretEnv)
	if len(s) < MinSecretLen {
		return nil, fmt.Errorf("env %s must hold a shared secret of at least %d bytes", c.SecretEnv, MinSecretLen)
	}
	return []byte(s), nil
}
