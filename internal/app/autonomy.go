package app

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Agent autonomy (node/autonomy.go): a project's mode, hop limit and budgets
// live in its binding and apply to its running node at once; the emergency
// stop (settings.Settings.StopAll) applies to every context.

// AutonomyView is a project's autonomy as the web UI shows it.
type AutonomyView struct {
	Mode string `json:"mode"` // settings.Autonomy*
	// Stopped is this project's manual agent pause; the global stop and
	// budget pause have separate switches.
	Stopped bool `json:"stopped"`
	// Halted: no agent of this project works by itself now (the global stop
	// or this project's pause is on).
	Halted bool `json:"halted,omitempty"`
	// MaxAutoDepth is the hop limit in effect (0: none); Default reports that
	// it follows the mode (none set).
	MaxAutoDepth        int  `json:"max_auto_depth"`
	MaxAutoDepthDefault bool `json:"max_auto_depth_default"`
	TurnsPerHour        int  `json:"turns_per_hour"`
	MaxRunMinutes       int  `json:"max_run_minutes"`
	// Paused: a budget of full mode ran out (PauseReason "turns" or "run");
	// autonomous delivery waits for the owner (autonomy/resume).
	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
	// TurnsLastHour and RunMinutes are the budgets used, in full mode.
	TurnsLastHour int `json:"turns_last_hour"`
	RunMinutes    int `json:"run_minutes"`
}

// autonomyOf is the node autonomy of binding b.
func autonomyOf(b settings.ProjectBinding) node.Autonomy {
	return node.Autonomy{Mode: b.AutonomyOf(), MaxDepth: b.MaxAutoDepthOf(), TurnsPerHour: b.TurnsPerHourOf(),
		MaxRun: time.Duration(b.MaxRunMinutesOf()) * time.Minute}
}

// ownAutonomy reports whether b sets any autonomy field of its own.
func ownAutonomy(b settings.ProjectBinding) bool {
	return b.Autonomy != "" || b.MaxAutoDepth != nil || b.TurnsPerHour != 0 || b.MaxRunMinutes != 0
}

// withAutonomy is b with the autonomy fields of src.
func withAutonomy(b, src settings.ProjectBinding) settings.ProjectBinding {
	b.Autonomy, b.MaxAutoDepth, b.TurnsPerHour, b.MaxRunMinutes = src.Autonomy, src.MaxAutoDepth, src.TurnsPerHour, src.MaxRunMinutes
	return b
}

// autonomyBaseLocked is the network binding whose autonomy a local project
// without its own follows: parent when it names a network binding, else the
// network binding bound to dir (the deepest folder containing it).
func (a *App) autonomyBaseLocked(parent, dir string) (settings.ProjectBinding, bool) {
	var base settings.ProjectBinding
	found := false
	for _, nb := range a.s.Bindings {
		if nb.ScopeOf() != settings.ProjectScopeNetwork {
			continue
		}
		if parent != "" && nb.ID == parent {
			return nb, true
		}
		if dir != "" && nb.Dir != "" && within(nb.Dir, dir) && (!found || len(nb.Dir) > len(base.Dir)) {
			base, found = nb, true
		}
	}
	return base, found
}

// effectiveBindingLocked is b with the autonomy in effect: a local project
// without its own reads it through from the network project bound to the same
// folder (autonomyBaseLocked), so the owner's later changes there apply too;
// with none bound, b's own defaults. A local chat (b.Chat, no Dir of its own)
// follows the local project of its folder, and so what that one follows; a
// folderless chat the network project bound to its folder.
func (a *App) effectiveBindingLocked(b settings.ProjectBinding) settings.ProjectBinding {
	if b.ScopeOf() != settings.ProjectScopeLocal || ownAutonomy(b) {
		return b
	}
	parent, dir := "", b.Dir
	if b.Chat != nil {
		if i := a.bindingIndex(b.Chat.Project); i >= 0 && a.s.Bindings[i].Chat == nil {
			return withAutonomy(b, a.effectiveBindingLocked(a.s.Bindings[i]))
		}
		parent, dir = b.Chat.Project, b.Chat.Folder
	}
	if base, ok := a.autonomyBaseLocked(parent, dir); ok {
		return withAutonomy(b, base)
	}
	return b
}

// reapplyAutonomyLocked applies the autonomy in effect to every running
// project node (after a change a local project may inherit).
func (a *App) reapplyAutonomyLocked() {
	for _, b := range a.s.Bindings {
		if c := a.projects[b.ID]; c != nil {
			c.n.SetAutonomy(autonomyOf(a.effectiveBindingLocked(b)))
		}
	}
}

// autonomyViewOf is the view of b's autonomy, with the budget state of its
// running node c (nil: none).
func autonomyViewOf(b settings.ProjectBinding, c *appContext) *AutonomyView {
	v := &AutonomyView{Mode: b.AutonomyOf(), Stopped: b.StopAgents, MaxAutoDepth: b.MaxAutoDepthOf(), MaxAutoDepthDefault: b.MaxAutoDepth == nil,
		TurnsPerHour: b.TurnsPerHourOf(), MaxRunMinutes: b.MaxRunMinutesOf()}
	if c != nil {
		st := c.n.AutonomyStatus()
		v.Halted, v.Paused, v.PauseReason = st.Stopped, st.Paused, st.Reason
		v.TurnsLastHour, v.RunMinutes = st.TurnsLastHour, int(st.Run/time.Minute)
	}
	return v
}

// ContextAutonomy is the autonomy state of one running context (Status): a
// folder bound to a local and a network project runs two, each with its own
// mode and budget pause; the global stop halts them all.
type ContextAutonomy struct {
	Project string `json:"project"` // LegacyProjectID for the legacy network
	Scope   string `json:"scope"`   // settings.ProjectScope*, "legacy"
	Dir     string `json:"dir,omitempty"`
	Mode    string `json:"mode"`
	// Halted: the global stop or the project's pause is on.
	Halted      bool   `json:"halted,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
	// Chat is set for a local chat that is not a folder's project chat.
	Chat *LocalChatView `json:"local_chat,omitempty"`
}

// contextAutonomyLocked lists the autonomy of every running context: the
// legacy network first, then the projects in binding order.
func (a *App) contextAutonomyLocked() []ContextAutonomy {
	var out []ContextAutonomy
	add := func(pid, scope, dir string, n *node.Node) {
		st := n.AutonomyStatus()
		out = append(out, ContextAutonomy{Project: pid, Scope: scope, Dir: dir, Mode: st.Mode,
			Halted: st.Stopped, Paused: st.Paused, PauseReason: st.Reason})
	}
	if a.legacy != nil {
		add(LegacyProjectID, "legacy", a.s.WorkDir, a.legacy.n)
	}
	var live map[string]map[string]bool
	for _, b := range a.s.Bindings {
		if c := a.projects[b.ID]; c != nil {
			add(b.ID, b.ScopeOf(), b.Dir, c.n)
			if b.Chat != nil {
				if live == nil {
					live = a.liveAgentsLocked()
				}
				out[len(out)-1].Chat = a.localChatViewLocked(b, live)
			}
		}
	}
	return out
}

// autonomyRequest is the autonomy part of POST projects/{pid}/binding
// (absent: kept). A negative max_auto_depth follows the mode again.
type autonomyRequest struct {
	Autonomy      *string `json:"autonomy"`
	MaxAutoDepth  *int    `json:"max_auto_depth"`
	TurnsPerHour  *int    `json:"turns_per_hour"`
	MaxRunMinutes *int    `json:"max_run_minutes"`
}

func (r autonomyRequest) empty() bool {
	return r.Autonomy == nil && r.MaxAutoDepth == nil && r.TurnsPerHour == nil && r.MaxRunMinutes == nil
}

// setAutonomyLocked saves a project's autonomy and applies it to its running
// node; the legacy network has none (bad_request), an invalid value is
// autonomy.
func (a *App) setAutonomyLocked(pid string, req autonomyRequest) error {
	i := a.bindingIndex(pid)
	if i < 0 {
		return &settings.Problem{Key: "bad_request"}
	}
	// A local project that follows its network project starts from what it
	// follows: the owner's edit overrides from there.
	b := a.effectiveBindingLocked(a.s.Bindings[i])
	if req.Autonomy != nil {
		b.Autonomy = *req.Autonomy
		if b.Autonomy == "" {
			b.Autonomy = settings.AutonomyOff
		}
	}
	if req.MaxAutoDepth != nil {
		b.MaxAutoDepth = nil
		if d := *req.MaxAutoDepth; d >= 0 {
			b.MaxAutoDepth = &d
		}
	}
	if req.TurnsPerHour != nil {
		b.TurnsPerHour = *req.TurnsPerHour
	}
	if req.MaxRunMinutes != nil {
		b.MaxRunMinutes = *req.MaxRunMinutes
	}
	s := a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i] = b
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	a.reapplyAutonomyLocked() // this project, and the local ones that follow it
	a.log.Info("project autonomy", "project", pid, "mode", b.AutonomyOf(), "max_auto_depth", b.MaxAutoDepthOf(),
		"turns_per_hour", b.TurnsPerHourOf(), "max_run_minutes", b.MaxRunMinutesOf())
	return nil
}

// resumeAutonomy ends a budget pause of a project: POST projects/{pid}/autonomy/resume.
func (a *App) resumeAutonomy(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	a.mu.Lock()
	c := a.projects[pid]
	a.mu.Unlock()
	if c == nil {
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	}
	c.n.ResumeAutonomy()
	a.log.Info("autonomous delivery resumed by the owner", "project", pid)
	a.changed(pid)
	a.writeView(w, pid)
}

// autonomyPaused tells the owner once that a project's budget paused its
// autonomous delivery: a tray notification (Notify) and the page.
func (a *App) autonomyPaused(pid, reason string) {
	a.mu.Lock()
	name := pid
	if v, ok := a.projectViewLocked(pid); ok {
		name = cmp.Or(v.Display, pid)
	}
	notify := a.Notify
	a.mu.Unlock()
	a.log.Warn("project autonomy paused", "project", pid, "reason", reason)
	a.changed(pid)
	if notify != nil {
		notify(msg("autonomy.paused.title", nil), msg("autonomy.paused."+reason, map[string]string{"project": name}))
	}
}

// StopAll reports whether every project's agents are stopped.
func (a *App) StopAll() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s.StopAll
}

// SetStopAll turns the emergency stop on or off: saved, then combined with
// each project's manual stop before applying to its running node.
func (a *App) SetStopAll(on bool) error {
	a.mu.Lock()
	if a.s.StopAll == on {
		a.mu.Unlock()
		return nil
	}
	s := a.s
	s.StopAll = on
	if a.configured {
		if err := settings.Save(a.path, s); err != nil {
			a.mu.Unlock()
			return err
		}
	}
	a.s = s
	changed := a.StopAllChanged
	if a.legacy != nil {
		a.legacy.n.SetStopped(on)
	}
	for _, b := range a.s.Bindings {
		if c := a.projects[b.ID]; c != nil {
			c.n.SetStopped(on || b.StopAgents)
		}
	}
	a.mu.Unlock()
	a.log.Warn("emergency stop of the agents", "on", on)
	if changed != nil {
		changed(on)
	}
	a.events.publish("status", "projects", "settings")
	return nil
}

// stopProjectAgents serves POST projects/{pid}/autonomy/stop {"on"}.
// This is a local member preference, independent of the global and budget
// pauses; the legacy network has no project-specific switch.
func (a *App) stopProjectAgents(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	i := a.bindingIndex(pid)
	if i < 0 {
		a.mu.Unlock()
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	}
	if a.s.Bindings[i].StopAgents != req.On {
		s := a.s
		s.Bindings = slices.Clone(s.Bindings)
		s.Bindings[i].StopAgents = req.On
		if err := settings.Save(a.path, s); err != nil {
			a.mu.Unlock()
			a.failed(w, "save project agent pause", err)
			return
		}
		a.s = s
	}
	if c := a.projects[pid]; c != nil {
		c.n.SetStopped(a.s.StopAll || req.On)
	}
	a.mu.Unlock()
	a.changed(pid, "settings", "status")
	a.writeView(w, pid)
}

// setStopAll serves POST /ui/api/autonomy/stop {"on"} -> Status.
func (a *App) setStopAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	if err := a.SetStopAll(req.On); err != nil {
		a.log.Error("save emergency stop", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.save", nil))
		return
	}
	writeJSON(w, a.Status())
}
