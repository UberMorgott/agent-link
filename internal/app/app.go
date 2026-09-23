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
	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// App is the desktop runtime around one node.
type App struct {
	path  string
	token string
	log   *slog.Logger

	// SetAutostart applies the autostart choice on save; replaceable in tests.
	// Nil makes autostart unavailable (a settings file other than the default).
	SetAutostart func(enable bool) error
	// AutostartState reads whether Windows starts the app at sign-in: the Run
	// entry exists and Task Manager has not switched it off. Nil uses the
	// saved setting.
	AutostartState func() (bool, error)
	// AutostartChanged, if set, is told the new state after a settings save
	// switched autostart, so the tray checkbox follows the page.
	AutostartChanged func(on bool)
	// Worker tunes the job queue (timeouts, activity pacing); MaxJobs comes
	// from the settings.
	Worker worker.Options
	// QuitFunc ends the program; Quit calls it. Nil makes quitting unavailable.
	QuitFunc func()
	// Ifaces lists network interfaces for ZeroTier detection; replaceable in tests.
	Ifaces func() []settings.Iface
	// PickFolder shows the native folder dialog starting at start and returns
	// the chosen absolute path or ErrPickCancelled; replaceable in tests.
	PickFolder func(start, title string) (string, error)
	// PickFile shows the native file dialog for a program, starting in the
	// folder start; replaceable in tests.
	PickFile func(start, title, filterName, filterSpec string) (string, error)
	// Agents finds agent programs: on PATH, at AgentPath, or at their
	// well-known install locations; replaceable in tests.
	Agents settings.Finder
	// Discovery allows LAN discovery when the settings ask for it (the
	// default); tests turn it off.
	Discovery bool

	// Version is this build's version (selfupdate.Version). Exe is set by
	// SetExecutable. Relaunch starts the updated executable; nil disables
	// updates. Latest finds the newest release and whether it is newer than
	// the given version; replaceable in tests, like UpdateFirst and
	// UpdateEvery (0: a minute after start, then every 6 hours).
	Version                  string
	Exe                      string
	Relaunch                 func() error
	Latest                   func(ctx context.Context, current string) (Release, bool, error)
	UpdateFirst, UpdateEvery time.Duration
	// HookExe is the executable the folder hooks run (`HookExe hook claude`);
	// an update replaces that file in place, so the path stays valid. Empty:
	// the app installs no hooks (tests, a settings file other than the default).
	HookExe string
	upd     updater
	events  *eventBroadcaster

	picking atomic.Bool    // a Windows dialog is open
	saves   sync.WaitGroup // background saves of a rediscovered agent path

	mu         sync.Mutex
	s          settings.Settings
	configured bool
	n          *node.Node
	stop       context.CancelFunc
	wg         sync.WaitGroup
	startErr   error
	listen     string // this side's address to give the others, of the last start
	zeroTier   bool
	hookClient string            // agent of the last hook sync, "" for none
	hookStates map[string]string // clean folder -> Hook* state of the last sync
}

// Status is a snapshot for the tray and the web UI.
type Status struct {
	Configured bool   `json:"configured"`
	Running    bool   `json:"running"`
	Node       string `json:"node"`
	// Peer is the first member with a session, else the first one known.
	Peer string `json:"peer"`
	// Connected: a session with at least one member.
	Connected bool   `json:"connected"`
	Handler   string `json:"handler"`
	// Online and Total count the other members (not removed).
	Online int `json:"online"`
	Total  int `json:"total"`
	// Members lists this node first, then the others (node.Members).
	Members []node.MemberInfo `json:"members,omitempty"`
	// Listen is this side's peer address, what the others type in.
	Listen   string `json:"listen,omitempty"`
	ZeroTier bool   `json:"zerotier"`
	// Problem is a strings key ("link.no_code", "link.no_peer", "link.bad_code", ...).
	Problem string `json:"problem,omitempty"`
	// Error is a start failure as one sentence for the user.
	Error string `json:"error,omitempty"`
	// Warning is a strings key shown next to any state: "link.weak_code" for
	// a legacy 6-character code while the listener is reachable beyond
	// private networks.
	Warning string `json:"warning,omitempty"`
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
		SetAutostart: setAutostart, AutostartState: autostartEnabled,
		Ifaces: settings.SystemIfaces, PickFolder: pickFolder, PickFile: pickFile,
		Agents: settings.SystemFinder, zeroTier: true, Discovery: true,
		Version: selfupdate.Version, Latest: latestRelease,
		events: newEventBroadcaster(),
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
// The node keeps running after ctx is cancelled, until Stop or the next Apply.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if !a.configured {
		a.mu.Unlock()
		return nil
	}
	err := a.startLocked(ctx)
	a.syncHooksLocked()
	a.mu.Unlock()
	a.events.publish("status", "dashboard", "participants")
	return err
}

// Stop stops the node and the worker and waits for them.
func (a *App) Stop() {
	a.mu.Lock()
	a.stopLocked()
	a.mu.Unlock()
	a.saves.Wait()
	a.events.publish("status", "dashboard", "participants")
}

// Status returns the current state.
func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Configured: a.configured, Running: a.n != nil,
		Node: a.s.Node, Handler: a.s.Handler,
		Listen: a.listen, ZeroTier: a.zeroTier,
	}
	for _, p := range a.s.Peers {
		if st.Peer == "" {
			st.Peer = p.Name
		}
	}
	if a.n != nil {
		if peers := a.n.Peers(); len(peers) > 0 {
			st.Peer = peers[0] // connected ones first
		}
		st.Connected = st.Peer != "" && a.n.Connected(st.Peer)
		st.Members = a.n.Members()
		for _, m := range st.Members[1:] {
			st.Total++
			if m.Online {
				st.Online++
			}
		}
	}
	switch {
	case a.n != nil && errors.Is(a.n.Problem(), node.ErrRemoved):
		st.Problem = "link.removed"
	case a.n != nil && errors.Is(a.n.Problem(), node.ErrNameTaken):
		st.Problem = "link.name_taken"
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
	case a.n != nil && errors.Is(a.n.Problem(), node.ErrLegacyRefused):
		st.Problem = "link.legacy_public"
	case len(a.s.Peers) == 0 && st.Total == 0:
		st.Problem = "link.no_peer"
	}
	if a.configured && config.WeakCode(a.s.Code) && config.ExposedListen(a.s.BindAddr()) {
		st.Warning = "link.weak_code"
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
// node. HandlerCommand, an empty API, absent peers, discovery and auto_update
// and, while no code is set, the legacy secret are kept from the current
// settings. A code replaces the secret.
// When no AgentPath is set, or the set one has disappeared (an app update
// moved its versioned folder), the agent is looked for again (Finder.Discover);
// a hit off PATH is saved as AgentPath and returned.
func (a *App) Apply(ctx context.Context, s settings.Settings) (found settings.Found, err error) {
	found, err = a.apply(ctx, s)
	// The settings are durable even when the restarted node reports a start
	// error, so every browser must refresh its saved values and status.
	if err == nil || errors.Is(err, ErrNotStarted) {
		a.events.publish("settings", "status", "dashboard", "participants", "update")
	}
	return found, err
}

func (a *App) apply(ctx context.Context, s settings.Settings) (found settings.Found, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.Peers == nil {
		// The page manages members with their own buttons; a legacy
		// peer_addr in the request is added to the kept list by Normalize.
		s.Peers = a.s.Peers
	}
	if s.Discovery == nil {
		s.Discovery = a.s.Discovery
	}
	if s.Projects == nil {
		// The page always sends its «Проекты» ({} when empty); a request
		// without the field keeps them.
		s.Projects = a.s.Projects
	}
	s = s.Normalize()
	s.HandlerCommand, s.Secret = a.s.HandlerCommand, ""
	if s.API == "" {
		s.API = a.s.API
	}
	if s.Code == "" {
		s.Secret = a.s.Secret
	}
	if s.AutoUpdate == nil {
		s.AutoUpdate = a.s.AutoUpdate
	}
	if s.AutoAnswer == nil {
		s.AutoAnswer = a.s.AutoAnswer
	}
	if len(s.HandlerCommand) == 0 && a.Agents.LookPath != nil && !a.agentPresent(s.AgentPath) {
		if f, ok := a.Agents.Discover(s.Handler); ok {
			s.AgentPath, found = "", f
			if f.Kind != settings.KindPath {
				s.AgentPath = f.Path
			}
		}
	}
	if err := settings.Save(a.path, s); err != nil {
		return settings.Found{}, err
	}
	if a.SetAutostart != nil && (s.Autostart != a.autostartLocked() || !a.configured) {
		if err := a.SetAutostart(s.Autostart); err != nil {
			a.log.Warn("autostart", "err", err)
		} else if a.AutostartChanged != nil {
			a.AutostartChanged(s.Autostart)
		}
	}
	a.stopLocked()
	a.s, a.configured = s, true
	a.syncHooksLocked()
	if err := a.startLocked(ctx); err != nil {
		return found, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	return found, nil
}

// agentPresent reports a set AgentPath that is a file.
func (a *App) agentPresent(p string) bool {
	if p == "" || a.Agents.Stat == nil {
		return false
	}
	st, err := a.Agents.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// agentCommand returns cmd for each job; the job runs it detached, so it
// survives a restart of the app. When cmd is the saved AgentPath and that file
// is gone by the time a job runs, the agent is looked for again, the job runs
// the new program and the new path is saved in the background.
func (a *App) agentCommand(cmd worker.Command, handler string, fromSetting bool) func() worker.Command {
	if !fromSetting || a.Agents.Stat == nil {
		return func() worker.Command { return cmd }
	}
	var mu sync.Mutex
	name := cmd.Name
	return func() worker.Command {
		mu.Lock()
		defer mu.Unlock()
		if !a.agentPresent(name) {
			if f, ok := a.Agents.Discover(handler); ok {
				old, saved := name, f.Path
				if f.Kind == settings.KindPath {
					saved = ""
				}
				a.log.Info("agent program moved", "old", old, "new", f.Path, "kind", f.Kind)
				name = f.Path
				a.saves.Go(func() { a.saveAgentPath(handler, old, saved) })
			}
		}
		c := cmd
		c.Name = name
		return c
	}
}

// saveAgentPath replaces the stored AgentPath old with p, unless the settings
// changed meanwhile. The running node keeps going: its runner already uses p.
func (a *App) saveAgentPath(handler, old, p string) {
	a.mu.Lock()
	if !a.configured || a.s.Handler != handler || a.s.AgentPath != old || len(a.s.HandlerCommand) > 0 {
		a.mu.Unlock()
		return
	}
	s := a.s
	s.AgentPath = p
	if err := settings.Save(a.path, s); err != nil {
		a.log.Warn("save rediscovered agent path", "err", err)
		a.mu.Unlock()
		return
	}
	a.s = s
	a.mu.Unlock()
	a.events.publish("settings")
}

func (a *App) startLocked(ctx context.Context) error {
	a.startErr = a.startNode(ctx)
	if a.startErr != nil {
		a.log.Error("node start", "err", a.startErr)
	}
	return a.startErr
}

// startNode runs the node, unless there is no code yet: then nothing can
// authenticate and Status asks for one. The node outlives ctx (often an HTTP
// request): only its values are kept, stopLocked ends the run.
func (a *App) startNode(ctx context.Context) error {
	var ifaces []settings.Iface
	if a.Ifaces != nil {
		ifaces = a.Ifaces()
	}
	a.listen, a.zeroTier = a.s.AdvertiseAddr(ifaces)
	key := a.s.Key()
	if key == nil {
		a.log.Info("node not started: no pairing code")
		return nil
	}
	cfg := a.s.NodeConfig(a.path, a.s.BindAddr())
	cfg.Discovery = cfg.Discovery && a.Discovery
	n, err := node.New(cfg, key, a.log)
	if err != nil {
		return err
	}
	n.SetAppVersion(a.Version)
	n.SetChangeHook(func(topic string) { a.events.publish(topic) })
	// Sessions bind to areas by folder: a project folder is its area's, the
	// working folder the one of direct messages.
	projects := map[string]string{}
	for area, p := range a.s.Projects {
		projects[area] = p.Dir
	}
	n.SetFolders(a.s.WorkDir, projects)
	// The job store always opens: with no handler, jobs left from an earlier
	// handler fail with a reply, and new requests stay manual.
	opt := a.Worker
	opt.MaxJobs = a.s.MaxJobs
	opt.OnChange = func() { a.events.publish("worker") }
	// Chat requests run in per-chat agent sessions and answer the whole chat.
	opt.Chats, opt.Self = n, cfg.Node
	// Agents reach this node's API through $AGENTLINK_API.
	opt.API = cfg.API
	// The handler answers only with auto-answer on, and then only requests no
	// live session takes (node.LiveSession); otherwise messages wait unread.
	cmd, hasHandler := a.s.Command()
	hasHandler = hasHandler && a.s.AutoAnswerOn()
	if hasHandler {
		opt.Agent = a.agentCommand(cmd, a.s.Handler, a.s.AgentPath != "" && len(a.s.HandlerCommand) == 0)
		// Requests addressed to an area with a project run there (see Settings.Projects).
		opt.Project = a.s.Project
	}
	w, err := worker.New(nil, n.SendMessage, cfg.DataDir, a.s.WorkDir, opt, a.log)
	if err != nil {
		return err
	}
	// Without a handler (or with auto-answer off) the hook only holds chat
	// requests past the chain limit; the rest waits unread for a session.
	if hasHandler {
		n.SetInboundHook(w.Accept)
	} else {
		n.SetInboundHook(w.ChatsOnly)
	}
	// A request answered here by hand or by an interactive session stops its job.
	n.SetLocalReplyHook(func(id string) { w.Answered(id) })
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
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
