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
	"time"

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

	mu         sync.Mutex
	s          settings.Settings
	configured bool
	n          *node.Node
	stop       context.CancelFunc
	wg         sync.WaitGroup
	startErr   error
}

// Status is a snapshot for the tray and the web UI.
type Status struct {
	Configured bool   `json:"configured"`
	Running    bool   `json:"running"`
	Node       string `json:"node"`
	Peer       string `json:"peer"`
	Connected  bool   `json:"connected"`
	Handler    string `json:"handler"`
	Error      string `json:"error,omitempty"`
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
	}, nil
}

// APIAddr is the loopback address the web UI and control API listen on.
func (a *App) APIAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s.APIAddr()
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
	}
	if a.n != nil {
		st.Connected = a.n.Connected(a.s.PeerName)
	}
	if a.startErr != nil {
		st.Error = a.startErr.Error()
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
// node. Hidden fields (API, HandlerCommand) are kept from the current settings.
func (a *App) Apply(s settings.Settings) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s.API, s.HandlerCommand = a.s.API, a.s.HandlerCommand
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
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
		return fmt.Errorf("%w: %v", ErrNotStarted, err)
	}
	return nil
}

func (a *App) startLocked() error {
	a.startErr = a.startNode()
	return a.startErr
}

func (a *App) startNode() error {
	cfg := a.s.NodeConfig(a.path)
	n, err := node.New(cfg, []byte(a.s.Secret), a.log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if cmd, ok := a.s.Command(); ok {
		w := worker.New(cmd.Runner(), n.Send, a.s.WorkDir, a.HandlerTimeout, a.log)
		n.SetInboundHook(w.Offer)
		a.wg.Go(func() { w.Run(ctx) })
	}
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
