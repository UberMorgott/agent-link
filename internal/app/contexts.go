package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// The app runs one node per context (docs/plans/projects-v1.md §6): the
// legacy network while a pairing code exists, and one per project binding,
// all on one node.Hub that owns the peer listener and discovery. Every worker
// takes its job slots from one shared worker.Slots.

// appContext is one context on the Hub: its node and, unless it is a project
// without a folder, its worker.
type appContext struct {
	pid    string // project id; "" for the legacy network
	n      *node.Node
	w      *worker.Worker     // nil: a project without a folder, where no agent runs
	cancel context.CancelFunc // stops the worker's Run
	done   chan struct{}      // closed once the worker's Run returned
}

// ErrUnknownProject: no binding (or no legacy network) has that id.
var ErrUnknownProject = errors.New("no such project")

// projectTopic is the event topic of a context: "project:<pid>", the legacy
// network's "project:legacy".
func projectTopic(pid string) string {
	if pid == "" {
		pid = LegacyProjectID
	}
	return "project:" + pid
}

// dataRoot is the legacy context's data directory, next to the settings file.
func (a *App) dataRoot() string { return filepath.Join(filepath.Dir(a.path), "data") }

// projectsRoot holds one data directory per project and .left/.
func (a *App) projectsRoot() string { return filepath.Join(a.dataRoot(), "projects") }

func (a *App) projectDir(pid string) string { return filepath.Join(a.projectsRoot(), pid) }

// bindingIndex is the position of pid in the bindings, or -1.
func (a *App) bindingIndex(pid string) int {
	return slices.IndexFunc(a.s.Bindings, func(b settings.ProjectBinding) bool { return b.ID == pid })
}

// startHubLocked binds the peer listener and starts the Hub and a closed
// slot pool; startNode opens it once every worker has reattached.
func (a *App) startHubLocked(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", a.s.BindAddr())
	if err != nil {
		return err
	}
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	hub := node.NewHub(ln, node.HubConfig{Discovery: a.s.DiscoveryOn() && a.Discovery, Log: a.log})
	hub.Start(hctx)
	jobs := a.s.MaxJobs
	if jobs <= 0 {
		jobs = worker.DefaultMaxJobs
	}
	a.hub, a.hubCtx, a.hubStop, a.slots = hub, hctx, cancel, worker.NewSlots(jobs)
	a.projects = map[string]*appContext{}
	a.log.Info("hub started", "listen", ln.Addr())
	return nil
}

// workerOptions are the options of a context's worker; hasHandler reports
// whether its agent answers requests (a handler with auto-answer on).
func (a *App) workerOptions(pid string, n *node.Node) (opt worker.Options, hasHandler bool) {
	opt = a.Worker
	opt.MaxJobs = a.s.MaxJobs
	topic := projectTopic(pid)
	opt.OnChange = func() { a.events.publish("worker", topic) }
	// Chat requests run in per-chat agent sessions and answer the whole chat.
	opt.Chats, opt.Self = n, a.s.Node
	// Agents reach the app's control API through $AGENTLINK_API, and name
	// their project through $AGENTLINK_PROJECT_ID.
	opt.API, opt.ProjectID, opt.Slots = a.s.APIAddr(), pid, a.slots
	// The handler answers only with auto-answer on, and then only requests no
	// live session takes (node.LiveSession); otherwise messages wait unread.
	cmd, hasHandler := a.s.Command()
	hasHandler = hasHandler && a.s.AutoAnswerOn()
	if hasHandler {
		opt.Agent = a.agentCommand(cmd, a.s.Handler, a.s.AgentPath != "" && len(a.s.HandlerCommand) == 0)
	}
	return opt, hasHandler
}

// wireWorker makes w the inbound hook of n.
func wireWorker(n *node.Node, w *worker.Worker, hasHandler bool) {
	// Without a handler (or with auto-answer off) the hook only holds chat
	// requests past the chain limit; the rest waits unread for a session.
	if hasHandler {
		n.SetInboundHook(w.Accept)
	} else {
		n.SetInboundHook(w.ChatsOnly)
	}
	// Peers show whether this node's worker answers when no session is open.
	n.SetAutoAnswer(hasHandler)
	// A request answered here by hand or by an interactive session stops its job.
	n.SetLocalReplyHook(func(id string) { w.Answered(id) })
}

// newNodeOf opens a context's node with the app's version and change events.
func (a *App) newNodeOf(pid string, cfg config.Config, key []byte) (*node.Node, error) {
	n, err := node.New(cfg, key, a.log)
	if err != nil {
		return nil, err
	}
	n.SetAppVersion(a.Version)
	if a.Waker != nil {
		n.SetSessionWaker(a.Waker)
	}
	if a.Poster != nil {
		n.SetInboxPoster(a.Poster)
	}
	if a.Launcher != nil {
		// A new session is of the handler's agent (Claude unless it is Codex);
		// a known last session of the folder is resumed in its own agent.
		n.SetLauncher(a.Launcher, a.s.Handler)
	}
	topic := projectTopic(pid)
	n.SetChangeHook(func(t string) { a.events.publish(t, topic) })
	// The emergency stop is the app's, for every context (SetStopAll).
	n.SetStopped(a.s.StopAll)
	// So is this member's chat color, which its record carries to the members.
	n.SetChatColor(a.s.ChatColor)
	n.SetDisplay(a.s.Nickname, a.s.NicknameAliases)
	n.SetAutonomyPauseHook(func(reason string) { go a.autonomyPaused(pid, reason) })
	return n, nil
}

// newLegacyContext opens the legacy network's node and worker (not running yet).
func (a *App) newLegacyContext(key []byte) (*appContext, error) {
	cfg := a.s.NodeConfig(a.path, a.s.BindAddr())
	cfg.Discovery = cfg.Discovery && a.Discovery
	n, err := a.newNodeOf("", cfg, key)
	if err != nil {
		return nil, err
	}
	// Sessions bind to areas by folder: a project folder is its area's, the
	// working folder the one of direct messages.
	projects := map[string]string{}
	for area, p := range a.s.Projects {
		projects[area] = p.Dir
	}
	n.SetFolders(a.s.WorkDir, projects)
	// The job store always opens: with no handler, jobs left from an earlier
	// handler fail with a reply, and new requests stay manual.
	opt, hasHandler := a.workerOptions("", n)
	opt.ProjectID = ""
	if hasHandler {
		// Requests addressed to an area with a project run there (see Settings.Projects).
		opt.Project = a.s.Project
	}
	w, err := worker.New(nil, n.SendMessage, cfg.DataDir, a.s.WorkDir, opt, a.log)
	if err != nil {
		return nil, err
	}
	wireWorker(n, w, hasHandler)
	return &appContext{n: n, w: w}, nil
}

// newProjectContext opens the node of project binding b and, when b has a
// folder, its worker there (not running yet). Without a folder the node has
// no folder at all and holds every request that asks it (HoldWithoutFolder).
func (a *App) newProjectContext(b settings.ProjectBinding) (*appContext, error) {
	key, err := config.ProjectKey(b.ID, b.Epoch, b.Secret)
	if err != nil {
		return nil, err
	}
	cfg := config.Config{Node: a.s.Node, Listen: a.s.BindAddr(), API: a.s.APIAddr(), DataDir: a.projectDir(b.ID),
		SecretEnv: "-", Discovery: a.s.DiscoveryOn() && a.Discovery, Project: b.ID}
	for _, p := range b.Peers {
		cfg.Peers = append(cfg.Peers, config.Peer{Addr: p})
	}
	n, err := a.newNodeOf(b.ID, cfg, key)
	if err != nil {
		return nil, err
	}
	c := &appContext{pid: b.ID, n: n}
	n.SetFolders(b.Dir, nil)
	n.SetAutonomy(autonomyOf(b))
	if b.Dir == "" {
		n.SetInboundHook(n.HoldWithoutFolder)
		return c, nil
	}
	n.SetLaunchMode(b.LaunchModeOf())
	opt, hasHandler := a.workerOptions(b.ID, n)
	if c.w, err = worker.New(nil, n.SendMessage, cfg.DataDir, b.Dir, opt, a.log); err != nil {
		return nil, err
	}
	wireWorker(n, c.w, hasHandler)
	return c, nil
}

// reattach registers the context's running jobs with the shared slots.
func (a *App) reattach(c *appContext) {
	if c.w != nil {
		c.w.Reattach(a.hubCtx)
	}
}

// runContextLocked runs c's node on the Hub and its worker.
func (a *App) runContextLocked(c *appContext) error {
	if err := a.hub.Add(c.n); err != nil {
		return err
	}
	if c.w != nil {
		wctx, cancel := context.WithCancel(a.hubCtx)
		c.cancel, c.done = cancel, make(chan struct{})
		go func() {
			defer close(c.done)
			c.w.Run(wctx)
		}()
	}
	if c.pid == "" {
		a.legacy, a.n = c, c.n
	} else {
		a.projects[c.pid] = c
	}
	return nil
}

// startContextLocked opens and runs a context added while the Hub runs: its
// worker reattaches first; the shared slots are open already.
func (a *App) startContextLocked(c *appContext, err error) error {
	if err != nil {
		return err
	}
	a.reattach(c)
	return a.runContextLocked(c)
}

// stopContextLocked stops c's node and worker and waits for them.
func (a *App) stopContextLocked(c *appContext) {
	a.hub.Remove(c.pid)
	if c.cancel != nil {
		c.cancel()
		<-c.done
	}
	if c.pid == "" {
		a.legacy, a.n = nil, nil
	} else {
		delete(a.projects, c.pid)
	}
}

// reportOrphanProjectsLocked logs the data of every project without a
// binding and leaves it where it is: a binding missing from the settings read
// at start (a leave that stopped before moving the data, a file restored or
// edited by hand, or a settings view that is not the real one, as under a
// packaged app's AppData virtualization) must never hide a project's data.
// Putting the binding back brings the project back as it was; joining the
// project again moves the old data to .left first (addProjectLocked).
func (a *App) reportOrphanProjectsLocked() {
	entries, err := os.ReadDir(a.projectsRoot())
	if err != nil {
		return // none yet
	}
	for _, e := range entries {
		if e.IsDir() && config.ValidProjectID(e.Name()) && a.bindingIndex(e.Name()) < 0 {
			a.log.Warn("project data without a binding kept", "project", e.Name(), "dir", a.projectDir(e.Name()), "settings", a.path)
		}
	}
}

// moveToLeft moves a project's data directory to .left/<pid>-<unix>.
func (a *App) moveToLeft(pid string) error {
	dir := a.projectDir(pid)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	left := filepath.Join(a.projectsRoot(), ".left")
	if err := os.MkdirAll(left, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(left, pid+"-"+strconv.FormatInt(time.Now().Unix(), 10))
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			break
		}
		dst = filepath.Join(left, fmt.Sprintf("%s-%d-%d", pid, time.Now().Unix(), i))
	}
	return os.Rename(dir, dst)
}

// LeaveProject leaves project pid (D8): the worker's intake closes unless it
// has unfinished jobs (worker.ErrBusy), the binding is removed from the
// settings, every member is told (a left tombstone), the context stops and
// its data moves to .left. Leaving the legacy network (LegacyProjectID)
// removes its code and secret and keeps its data.
func (a *App) LeaveProject(pid string) error {
	err := a.leaveProject(pid)
	if err == nil {
		a.events.publish("projects", projectTopic(pid), "status", "settings", "dashboard", "participants")
	}
	return err
}

func (a *App) leaveProject(pid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if pid == LegacyProjectID {
		return a.leaveLegacyLocked()
	}
	i := a.bindingIndex(pid)
	if i < 0 {
		return ErrUnknownProject
	}
	c := a.projects[pid]
	if err := quiesce(c); err != nil {
		return err
	}
	s := a.s
	s.Bindings = slices.Delete(slices.Clone(s.Bindings), i, i+1)
	if len(s.Bindings) == 0 {
		s.Bindings = nil
	}
	if err := settings.Save(a.path, s); err != nil {
		resume(c)
		return err
	}
	a.s = s
	if c != nil {
		if err := c.n.Leave(); err != nil {
			a.log.Warn("leave project", "project", pid, "err", err)
		}
		a.stopContextLocked(c)
	}
	if err := a.moveToLeft(pid); err != nil {
		a.log.Warn("move left project data", "project", pid, "err", err)
	}
	a.syncHooksLocked()
	a.log.Info("project left", "project", pid)
	return nil
}

// leaveLegacyLocked drops the legacy network's code and secret and stops it;
// its data stays.
func (a *App) leaveLegacyLocked() error {
	if a.s.Key() == nil {
		return ErrUnknownProject
	}
	c := a.legacy
	if err := quiesce(c); err != nil {
		return err
	}
	s := a.s
	s.Code, s.Secret = "", ""
	if err := settings.Save(a.path, s); err != nil {
		resume(c)
		return err
	}
	a.s = s
	if c != nil {
		a.stopContextLocked(c)
	}
	a.log.Info("legacy network left")
	return nil
}

// quiesce closes c's worker intake for a leave or a folder change, or
// reports worker.ErrBusy.
func quiesce(c *appContext) error {
	if c == nil || c.w == nil {
		return nil
	}
	return c.w.Quiesce()
}

// resume reopens the intake quiesce closed, when the change failed.
func resume(c *appContext) {
	if c != nil && c.w != nil {
		c.w.Resume()
	}
}

// setProjectDirLocked binds project pid (or the legacy network's working
// folder) to dir: the worker must have no unfinished jobs (worker.ErrBusy),
// the settings are saved, the context restarts in the new folder and the
// worker's agent sessions of the old folder are deleted, so the next request
// starts a fresh one there. dir must be validated by the caller.
func (a *App) setProjectDirLocked(pid, dir string) error {
	var c *appContext
	s := a.s
	if pid == LegacyProjectID {
		c = a.legacy
		s.WorkDir = dir
	} else {
		i := a.bindingIndex(pid)
		if i < 0 {
			return ErrUnknownProject
		}
		c = a.projects[pid]
		s.Bindings = slices.Clone(s.Bindings)
		s.Bindings[i].Dir = dir
	}
	if err := quiesce(c); err != nil {
		return err
	}
	if err := settings.Save(a.path, s); err != nil {
		resume(c)
		return err
	}
	a.s = s
	a.syncHooksLocked()
	stateDir := a.dataRoot()
	if pid != LegacyProjectID {
		stateDir = a.projectDir(pid)
	}
	if c != nil {
		a.stopContextLocked(c)
	}
	sessions, _ := filepath.Glob(filepath.Join(stateDir, "sessions", "*.json"))
	for _, f := range sessions {
		if err := os.Remove(f); err != nil {
			a.log.Warn("remove agent session of the old folder", "file", f, "err", err)
		}
	}
	if a.hub == nil {
		return nil
	}
	if pid == LegacyProjectID {
		if key := a.s.Key(); key != nil {
			return a.startContextLocked(a.newLegacyContext(key))
		}
		return nil
	}
	return a.startContextLocked(a.newProjectContext(a.s.Bindings[a.bindingIndex(pid)]))
}
