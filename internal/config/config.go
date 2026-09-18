// Package config loads and validates an agentlink node configuration.
package config

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MinSecretLen is the minimum accepted length of the shared secret in bytes.
const MinSecretLen = 16

// CodeLen is the length of a pairing code: letters and digits, case-insensitive.
const CodeLen = 6

// DefaultPort is the peer port used when an address has none.
const DefaultPort = 7420

// codeKDFInfo is the fixed HKDF context for keys derived from a pairing code.
const codeKDFInfo = "agentlink/pair-code/v1"

// Peer is a remote node this node dials. An empty Name is learned from the
// handshake: the peer announces it and the shared key authenticates it.
type Peer struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr"`
}

var codePattern = regexp.MustCompile(`^[A-Za-z0-9]{6}$`)

// NormalizeCode upper-cases a pairing code and reports whether it is exactly
// CodeLen ASCII letters or digits.
func NormalizeCode(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !codePattern.MatchString(s) {
		return "", false
	}
	return strings.ToUpper(s), true
}

// KeyFromCode derives the 32-byte session key from a pairing code. Case does
// not matter. A 6-character code is guessable offline by anyone who records a
// handshake, so the private network, not the code, is the security boundary.
func KeyFromCode(code string) ([]byte, error) {
	norm, ok := NormalizeCode(code)
	if !ok {
		return nil, fmt.Errorf("pairing code must be %d letters or digits", CodeLen)
	}
	return hkdf.Key(sha256.New, []byte(norm), nil, codeKDFInfo, 32)
}

// WithDefaultPort returns addr as host:port, adding DefaultPort when addr is
// a bare host or IP (IPv6 with or without brackets).
func WithDefaultPort(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = strings.Trim(addr, "[]"), strconv.Itoa(DefaultPort)
	}
	if host == "" || strings.ContainsAny(host, " /\\[]") {
		return "", fmt.Errorf("invalid address %q", addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return "", fmt.Errorf("invalid port in %q", addr)
	}
	return net.JoinHostPort(host, port), nil
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

var namePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}_-]{0,63}$`)

// ValidName reports whether s is usable as a node or area name: letters (any
// script), digits, "_" and "-", starting with a letter or digit, up to 64.
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
		case p.Name == "":
		case !ValidName(p.Name):
			errs = append(errs, fmt.Errorf("invalid peer name %q", p.Name))
		case p.Name == c.Node:
			errs = append(errs, fmt.Errorf("peer %q has this node's name", p.Name))
		case seen[p.Name]:
			errs = append(errs, fmt.Errorf("duplicate peer %q", p.Name))
		}
		if p.Name != "" {
			seen[p.Name] = true
		}
		if _, _, err := net.SplitHostPort(p.Addr); err != nil {
			errs = append(errs, fmt.Errorf("peer %q: invalid addr %q", p.Name, p.Addr))
		}
	}
	return errors.Join(errs...)
}

// Secret returns the session key from the environment variable named by
// SecretEnv: a 6-character pairing code (see KeyFromCode), or a long shared
// secret of at least MinSecretLen bytes used as is.
func (c Config) Secret() ([]byte, error) {
	s := os.Getenv(c.SecretEnv)
	if _, ok := NormalizeCode(s); ok {
		return KeyFromCode(s)
	}
	if len(s) < MinSecretLen {
		return nil, fmt.Errorf("env %s must hold a %d-character pairing code or a shared secret of at least %d bytes", c.SecretEnv, CodeLen, MinSecretLen)
	}
	return []byte(s), nil
}
