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

// autonomyViewOf is the view of b's autonomy, with the budget state of its
// running node c (nil: none).
func autonomyViewOf(b settings.ProjectBinding, c *appContext) *AutonomyView {
	v := &AutonomyView{Mode: b.AutonomyOf(), MaxAutoDepth: b.MaxAutoDepthOf(), MaxAutoDepthDefault: b.MaxAutoDepth == nil,
		TurnsPerHour: b.TurnsPerHourOf(), MaxRunMinutes: b.MaxRunMinutesOf()}
	if c != nil {
		st := c.n.AutonomyStatus()
		v.Paused, v.PauseReason = st.Paused, st.Reason
		v.TurnsLastHour, v.RunMinutes = st.TurnsLastHour, int(st.Run/time.Minute)
	}
	return v
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
	b := a.s.Bindings[i]
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
	if c := a.projects[pid]; c != nil {
		c.n.SetAutonomy(autonomyOf(b))
	}
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

// SetStopAll turns the emergency stop on or off: saved, then applied to every
// running context (node.SetStopped).
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
	var ctxs []*appContext
	if a.legacy != nil {
		ctxs = append(ctxs, a.legacy)
	}
	for _, c := range a.projects {
		ctxs = append(ctxs, c)
	}
	changed := a.StopAllChanged
	a.mu.Unlock()
	for _, c := range ctxs {
		c.n.SetStopped(on)
	}
	a.log.Warn("emergency stop of the agents", "on", on)
	if changed != nil {
		changed(on)
	}
	a.events.publish("status", "projects", "settings")
	return nil
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
