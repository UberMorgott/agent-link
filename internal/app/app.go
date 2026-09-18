// Package app runs an agentlink node for the desktop tray: it owns the
// settings file, restarts the node when settings change, runs the agent
// worker, and serves the local web UI next to the node's control API.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// App is the desktop runtime around one node.
type App struct {
	path  string
	token string
	log   *slog.Logger

	// SetAutostart applies the autostart choice on save; replaceable in tests.
	SetAutostart func(enable bool) error
	// HandlerTimeout bounds one agent run.
	HandlerTimeout time.Duration
	// QuitFunc ends the program; Quit calls it. Nil makes quitting unavailable.
	QuitFunc func()
	// Ifaces lists network interfaces for ZeroTier detection; replaceable in tests.
	Ifaces func() []settings.Iface
	// PickFolder shows the native folder dialog starting at start and returns
	// the chosen absolute path or ErrPickCancelled; replaceable in tests.
	PickFolder func(start, title string) (string, error)

	picking atomic.Bool // a folder dialog is open

	mu         sync.Mutex
	s          settings.Settings
	configured bool
	n          *node.Node
	stop       context.CancelFunc
	wg         sync.WaitGroup
	startErr   error
	listen     string // effective peer listener address of the last start
	zeroTier   bool
}

// Status is a snapshot for the tray and the web UI.
type Status struct {
	Configured bool   `json:"configured"`
	Running    bool   `json:"running"`
	Node       string `json:"node"`
	Peer       string `json:"peer"`
	Connected  bool   `json:"connected"`
	Handler    string `json:"handler"`
	// Listen is this side's peer address, what the other person types in.
	Listen   string `json:"listen,omitempty"`
	ZeroTier bool   `json:"zerotier"`
	// Problem is a strings key ("link.no_code", "link.no_peer", "link.bad_code", ...).
	Problem string `json:"problem,omitempty"`
	// Error is a start failure as one sentence for the user.
	Error string `json:"error,omitempty"`
}

// New loads settings from path. log may be nil.
func New(path string, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s, ok, err := settings.Load(path)
	if err != nil {
		return nil, err
	}
	return &App{
		path: path, token: newToken(), log: log, s: s, configured: ok,
		SetAutostart: setAutostart, HandlerTimeout: worker.DefaultTimeout,
		Ifaces: settings.SystemIfaces, PickFolder: pickFolder, zeroTier: true,
	}, nil
}

// APIAddr is the loopback address the web UI and control API listen on.
func (a *App) APIAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s.APIAddr()
}

// SetAPIAddr overrides the loopback address of the web UI and control API
// before the server starts; the next save persists it.
func (a *App) SetAPIAddr(addr string) error {
	if !config.IsLoopbackAddr(addr) {
		return fmt.Errorf("api %q must be a loopback host:port", addr)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.s.API = addr
	return nil
}

// Quit asks the program to exit through QuitFunc, if set.
func (a *App) Quit() {
	if a.QuitFunc != nil {
		a.QuitFunc()
	}
}

// Configured reports whether a settings file exists.
func (a *App) Configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configured
}

// Start runs the node if settings exist. A start failure is also kept for Status.
func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.configured {
		return nil
	}
	return a.startLocked()
}

// Stop stops the node and the worker and waits for them.
func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
}

// Status returns the current state.
func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Configured: a.configured, Running: a.n != nil,
		Node: a.s.Node, Peer: a.s.PeerName, Handler: a.s.Handler,
		Listen: a.listen, ZeroTier: a.zeroTier,
	}
	if a.n != nil {
		if peers := a.n.Peers(); len(peers) > 0 {
			st.Peer = peers[0]
		}
		st.Connected = st.Peer != "" && a.n.Connected(st.Peer)
	}
	switch {
	case !a.configured || st.Connected:
	case a.startErr != nil:
		st.Error = userError(a.startErr)
	case a.s.Key() == nil:
		st.Problem = "link.no_code"
	case a.n != nil && errors.Is(a.n.Problem(), node.ErrAuth):
		st.Problem = "link.bad_code"
	case a.n != nil && errors.Is(a.n.Problem(), node.ErrSameName):
		st.Problem = "link.same_name"
	case a.n != nil && errors.Is(a.n.Problem(), node.ErrWrongPeer):
		st.Problem = "link.wrong_peer"
	case a.s.PeerAddr == "":
		st.Problem = "link.no_peer"
	}
	return st
}

// Settings returns the current settings.
func (a *App) Settings() settings.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s
}

// ErrNotStarted wraps a node start failure after settings were saved.
var ErrNotStarted = errors.New("settings saved, but the node did not start")

// Apply validates and saves new settings, updates autostart and restarts the
// node. HandlerCommand, an empty API and, while no code is set, the legacy
// secret are kept from the current settings. A code replaces the secret.
func (a *App) Apply(s settings.Settings) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s = s.Normalize()
	s.HandlerCommand, s.Secret = a.s.HandlerCommand, ""
	if s.API == "" {
		s.API = a.s.API
	}
	if s.Code == "" {
		s.Secret = a.s.Secret
	}
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	if a.SetAutostart != nil && (s.Autostart != a.s.Autostart || !a.configured) {
		if err := a.SetAutostart(s.Autostart); err != nil {
			a.log.Warn("autostart", "err", err)
		}
	}
	a.stopLocked()
	a.s, a.configured = s, true
	if err := a.startLocked(); err != nil {
		return fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	return nil
}

func (a *App) startLocked() error {
	a.startErr = a.startNode()
	if a.startErr != nil {
		a.log.Error("node start", "err", a.startErr)
	}
	return a.startErr
}

// startNode runs the node, unless there is no code yet: then nothing can
// authenticate and Status asks for one.
func (a *App) startNode() error {
	var ifaces []settings.Iface
	if a.Ifaces != nil {
		ifaces = a.Ifaces()
	}
	a.listen, a.zeroTier = a.s.ListenAddr(ifaces)
	key := a.s.Key()
	if key == nil {
		a.log.Info("node not started: no pairing code")
		return nil
	}
	cfg := a.s.NodeConfig(a.path, a.listen)
	n, err := node.New(cfg, key, a.log)
	if err != nil {
		return err
	}
	// The job store always opens: with no handler, jobs left from an earlier
	// handler fail with a reply, and new requests stay manual (no hook).
	var run worker.Runner
	cmd, hasHandler := a.s.Command()
	if hasHandler {
		run = cmd.Runner()
	}
	w, err := worker.New(run, n.SendMessage, cfg.DataDir, a.s.WorkDir, a.HandlerTimeout, a.log)
	if err != nil {
		return err
	}
	if hasHandler {
		n.SetInboundHook(w.Accept)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.wg.Go(func() { w.Run(ctx) })
	a.wg.Go(func() { n.Run(ctx, ln) })
	a.n, a.stop = n, cancel
	a.log.Info("node started", "node", cfg.Node, "listen", ln.Addr(), "handler", a.s.Handler)
	return nil
}

func (a *App) stopLocked() {
	if a.stop != nil {
		a.stop()
		a.wg.Wait()
		a.log.Info("node stopped")
	}
	a.n, a.stop = nil, nil
}

func (a *App) node() *node.Node {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}
