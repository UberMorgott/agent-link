package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// The projects web API (docs/plans/projects-v1.md §7.2), under /ui/api/ with
// the page token. Every error is {"error": <sentence>, "code": <code>}.

// legacyName is the legacy network's name in the project list.
const legacyName = "Прежняя сеть"

// projectRoutes mounts the projects API on api.
func (a *App) projectRoutes(api *http.ServeMux) {
	const p = "/ui/api/projects"
	api.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.Projects()) })
	api.HandleFunc("POST "+p, a.createProject)
	api.HandleFunc("POST "+p+"/join", a.joinProject)
	api.HandleFunc("GET "+p+"/{pid}", func(w http.ResponseWriter, r *http.Request) { a.writeView(w, r.PathValue("pid")) })
	api.HandleFunc("POST "+p+"/{pid}/name", a.renameProject)
	api.HandleFunc("POST "+p+"/{pid}/binding", a.bindProject)
	api.HandleFunc("POST "+p+"/{pid}/invite", a.revealInvite)
	api.HandleFunc("POST "+p+"/{pid}/members/add", a.addProjectMember)
	api.HandleFunc("POST "+p+"/{pid}/members/remove", a.removeProjectMember)
	api.HandleFunc("POST "+p+"/{pid}/leave", a.leave)
	api.HandleFunc("GET "+p+"/{pid}/chats", a.projectChats)
	api.HandleFunc("POST "+p+"/{pid}/chats", a.newProjectChat)
	api.HandleFunc("GET "+p+"/{pid}/chats/{id}", a.projectChat(func(n *node.Node, r *http.Request, id string) (any, error) { return n.Chat(id) }))
	api.HandleFunc("GET "+p+"/{pid}/chats/{id}/messages", a.projectChat(chatMessages))
	api.HandleFunc("POST "+p+"/{pid}/chats/{id}/close", a.projectChat(func(n *node.Node, _ *http.Request, id string) (any, error) {
		return n.CloseChat(id)
	}))
	api.HandleFunc("POST "+p+"/{pid}/chats/{id}/archive", a.projectChat(func(n *node.Node, _ *http.Request, id string) (any, error) {
		return n.ArchiveChat(id)
	}))
	api.HandleFunc("POST "+p+"/{pid}/chats/{id}/members", a.projectChat(chatMembers))
	api.HandleFunc("POST "+p+"/{pid}/send", a.projectSend)
	api.HandleFunc("GET "+p+"/{pid}/seats", a.projectSeats(func(n *node.Node, _ *http.Request) (any, error) { return n.Seats(), nil }))
	api.HandleFunc("POST "+p+"/{pid}/seats", a.projectSeats(func(n *node.Node, r *http.Request) (any, error) {
		var req node.SeatRequest
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody)).Decode(&req); err != nil {
			return nil, node.ErrBadRequest
		}
		return n.AddSeat(req)
	}))
	api.HandleFunc("POST "+p+"/{pid}/seats/{sid}/start", a.projectSeats(func(n *node.Node, r *http.Request) (any, error) {
		var req struct {
			Open bool `json:"open"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody)).Decode(&req) // an empty body is fine
		return n.StartSeat(r.PathValue("sid"), req.Open)
	}))
	api.HandleFunc("POST "+p+"/{pid}/seats/{sid}/stop", a.projectSeats(func(n *node.Node, r *http.Request) (any, error) {
		return n.StopSeat(r.PathValue("sid"))
	}))
	api.HandleFunc("POST "+p+"/{pid}/seats/{sid}/remove", a.projectSeats(func(n *node.Node, r *http.Request) (any, error) {
		if err := n.RemoveSeat(r.PathValue("sid")); err != nil {
			return nil, err
		}
		return n.Seats(), nil
	}))
}

// projectSeats serves the local agents (seats) of project pid: 404 not_found
// for an unknown project or seat, 400 bad_request for a request the node
// refuses (no folder, a bad provider or label).
func (a *App) projectSeats(do func(n *node.Node, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, ok := a.contextNode(w, r.PathValue("pid"))
		if !ok {
			return
		}
		v, err := do(n, r)
		switch {
		case errors.Is(err, node.ErrUnknownSeat):
			writeCodedError(w, http.StatusNotFound, "not_found")
		case errors.Is(err, node.ErrNeedsFolder):
			writeCodedError(w, http.StatusBadRequest, "project_needs_folder")
		case err != nil:
			writeCodedError(w, http.StatusBadRequest, "bad_request")
		default:
			writeJSON(w, v)
		}
	}
}

// Projects lists every project by display name, the legacy network last.
func (a *App) Projects() []ProjectView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []ProjectView{}
	for _, b := range a.s.Bindings {
		if v, ok := a.projectViewLocked(b.ID); ok {
			out = append(out, v)
		}
	}
	slices.SortStableFunc(out, func(x, y ProjectView) int {
		return cmp.Or(cmp.Compare(strings.ToLower(x.Display), strings.ToLower(y.Display)), cmp.Compare(x.ID, y.ID))
	})
	if v, ok := a.projectViewLocked(LegacyProjectID); ok {
		out = append(out, v)
	}
	return out
}

// projectViewLocked builds the view of pid; ok is false for no such project.
func (a *App) projectViewLocked(pid string) (ProjectView, bool) {
	var v ProjectView
	var c *appContext
	if pid == LegacyProjectID {
		if a.s.Key() == nil {
			return ProjectView{}, false
		}
		c = a.legacy
		v = ProjectView{ID: pid, Legacy: true, Name: legacyName, Dir: a.s.WorkDir, HasInvite: a.s.Code != ""}
	} else {
		i := a.bindingIndex(pid)
		if i < 0 {
			return ProjectView{}, false
		}
		b := a.s.Bindings[i]
		c = a.projects[pid]
		v = ProjectView{ID: pid, Alias: b.Alias, Dir: b.Dir, CanRename: true, HasInvite: true, AutoOpen: b.AutoOpenOn(), LaunchMode: b.LaunchModeOf()}
		if c != nil {
			v.Name = c.n.ProjectMeta().Name
		}
	}
	v.Display = cmp.Or(v.Alias, v.Name)
	v.Members = []node.MemberInfo{{Name: a.s.Node, Self: true, Online: true}}
	if c != nil {
		v.Members = c.n.Members()
		v.Problem = problemCode(c.n.Problem())
		v.Busy = c.w != nil && c.w.Busy()
	}
	v.Online, v.Total = peerCounts(v.Members)
	switch {
	case v.Problem != "":
		v.State = ProjectError
	case v.Name == "":
		v.State = ProjectConnecting
	case v.Dir == "":
		v.State = ProjectNeedsFolder
	default:
		v.State = ProjectReady
	}
	return v, true
}

// peerCounts counts the other members of a member list: this node (Self,
// always online) is left out, so a project whose members are you and one
// online peer reads "online 1 of 1" and its dot goes on only with a peer.
func peerCounts(members []node.MemberInfo) (online, total int) {
	for _, m := range members {
		if m.Self {
			continue
		}
		total++
		if m.Online {
			online++
		}
	}
	return online, total
}

// problemCode is the ProjectView problem of a node's last handshake failure;
// timeouts, refused dials and the rest set none.
func problemCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, node.ErrAuth):
		return "auth"
	case errors.Is(err, node.ErrUnknownProject):
		return "unknown_project"
	case errors.Is(err, node.ErrWrongProject):
		return "wrong_project"
	case errors.Is(err, node.ErrRemoved):
		return "removed"
	case errors.Is(err, node.ErrNameTaken):
		return "name_taken"
	}
	return ""
}

func (a *App) writeView(w http.ResponseWriter, pid string) {
	a.mu.Lock()
	v, ok := a.projectViewLocked(pid)
	a.mu.Unlock()
	if !ok {
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, v)
}

// changed publishes a change of project pid's binding, meta or membership.
func (a *App) changed(pid string, more ...string) {
	a.events.publish(append([]string{"projects", projectTopic(pid)}, more...)...)
}

// decode reads a JSON body; a bad one answers 400 bad_request.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		writeCodedError(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

// failed answers err: a settings problem with its own code, else a logged
// internal error.
func (a *App) failed(w http.ResponseWriter, what string, err error) {
	var p *settings.Problem
	switch {
	case errors.As(err, &p) && p.Key == "too_many_projects":
		writeCodedError(w, http.StatusConflict, p.Key)
	case errors.As(err, &p):
		writeCodedError(w, http.StatusBadRequest, p.Key)
	case errors.Is(err, worker.ErrBusy):
		writeCodedError(w, http.StatusConflict, "project_busy")
	case errors.Is(err, ErrUnknownProject):
		writeCodedError(w, http.StatusNotFound, "not_found")
	default:
		a.log.Error(what, "err", err)
		writeCodedError(w, http.StatusInternalServerError, "internal")
	}
}

// checkDir validates a folder to bind ("" = none): an absolute existing
// directory that no other binding than pid's holds.
func (a *App) checkDirLocked(pid, dir string) error {
	if dir == "" {
		return nil
	}
	if st, err := os.Stat(dir); !filepath.IsAbs(dir) || err != nil || !st.IsDir() {
		return &settings.Problem{Key: "dir"}
	}
	for _, b := range a.s.Bindings {
		if b.ID != pid && b.Dir != "" && settings.DirKey(b.Dir) == settings.DirKey(dir) {
			return &settings.Problem{Key: "dir_taken"}
		}
	}
	return nil
}

// ensureHubLocked starts the Hub when a first context is added later.
func (a *App) ensureHubLocked(ctx context.Context) error {
	if a.hub != nil {
		return nil
	}
	if err := a.startHubLocked(ctx); err != nil {
		return err
	}
	a.slots.Open() // no worker to reattach yet
	return nil
}

// addProjectLocked starts a new project context for b: its data directory
// (and meta, when name is set: the creator's) first, then the settings.
func (a *App) addProjectLocked(ctx context.Context, b settings.ProjectBinding, name string) error {
	s := a.s
	s.Bindings = append(slices.Clone(s.Bindings), b)
	if err := s.Validate(); err != nil {
		return err
	}
	if err := a.ensureHubLocked(ctx); err != nil {
		return err
	}
	// Data of an earlier binding of this project (kept at start, see
	// reportOrphanProjectsLocked) goes to .left: a new incarnation never
	// inherits old queues, chats or node id.
	if err := a.moveToLeft(b.ID); err != nil {
		return err
	}
	c, err := a.newProjectContext(b)
	if err != nil {
		return err
	}
	if name != "" {
		if _, err := c.n.Rename(name); err != nil {
			return err
		}
	}
	// A failed save leaves the new data directory unbound; a later join of
	// the project moves it to .left.
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s, a.configured = s, true
	a.syncHooksLocked()
	return a.startContextLocked(c, nil) //nolint:contextcheck // the context outlives the request: it runs under the Hub's own
}

func (a *App) createProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Dir   string `json:"dir"`
		Alias string `json:"alias"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, err := node.NormalizeProjectName(req.Name)
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "name")
		return
	}
	alias, dir := strings.TrimSpace(req.Alias), strings.TrimSpace(req.Dir)
	if !settings.ValidAlias(alias) {
		writeCodedError(w, http.StatusBadRequest, "alias")
		return
	}
	pid, err := config.NewProjectID()
	if err != nil {
		a.failed(w, "new project id", err)
		return
	}
	secret, err := config.NewProjectSecret()
	if err != nil {
		a.failed(w, "new project secret", err)
		return
	}
	a.mu.Lock()
	err = a.checkDirLocked("", dir)
	if err == nil && len(a.s.Bindings) >= settings.MaxProjects {
		err = &settings.Problem{Key: "too_many_projects"}
	}
	if err == nil {
		b := settings.ProjectBinding{ID: pid, Epoch: config.ProjectEpoch, Secret: secret, Alias: alias, Dir: filepathClean(dir)}
		err = a.addProjectLocked(r.Context(), b, name)
	}
	v, _ := a.projectViewLocked(pid)
	a.mu.Unlock()
	if err != nil {
		a.failed(w, "create project", err)
		return
	}
	a.changed(pid)
	writeJSON(w, v)
}

func filepathClean(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Clean(dir)
}

func (a *App) joinProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Invite string `json:"invite"`
		Addr   string `json:"addr"`
		// Dir is the legacy network's working folder, for a code joined while
		// an agent answers (settings.Validate needs one then); an invite
		// ignores it.
		Dir string `json:"dir"`
	}
	if !decode(w, r, &req) {
		return
	}
	addr := strings.TrimSpace(req.Addr)
	if addr != "" {
		norm, err := config.WithDefaultPort(addr)
		if err != nil {
			writeCodedError(w, http.StatusBadRequest, "addr")
			return
		}
		addr = norm
	}
	dir := strings.TrimSpace(req.Dir)
	if dir != "" && !filepath.IsAbs(dir) {
		writeCodedError(w, http.StatusBadRequest, "work_dir")
		return
	}
	invite := strings.TrimSpace(req.Invite)
	if inv, err := config.ParseInvite(invite); err == nil {
		a.joinByInvite(w, r, inv, addr)
		return
	}
	if code, ok := config.NormalizeCode(invite); ok {
		a.joinLegacy(w, r, code, addr, filepathClean(dir))
		return
	}
	writeCodedError(w, http.StatusBadRequest, "invite")
}

// joinByInvite joins project inv. An invite of a project already here opens
// it (created: false) and changes nothing.
func (a *App) joinByInvite(w http.ResponseWriter, r *http.Request, inv config.Invite, addr string) {
	a.mu.Lock()
	var err error
	created := false
	switch i := a.bindingIndex(inv.ProjectID); {
	case i >= 0 && a.s.Bindings[i].Secret != inv.Secret:
		a.mu.Unlock()
		writeCodedError(w, http.StatusConflict, "conflict_secret")
		return
	case i >= 0:
	case len(a.s.Bindings) >= settings.MaxProjects:
		err = &settings.Problem{Key: "too_many_projects"}
	default:
		b := settings.ProjectBinding{ID: inv.ProjectID, Epoch: inv.Epoch, Secret: inv.Secret}
		if addr != "" {
			b.Peers = []string{addr}
		}
		err, created = a.addProjectLocked(r.Context(), b, ""), true
	}
	v, _ := a.projectViewLocked(inv.ProjectID)
	a.mu.Unlock()
	if err != nil {
		a.failed(w, "join project", err)
		return
	}
	if created {
		a.changed(inv.ProjectID)
	}
	writeJSON(w, JoinResult{Project: v, Created: created})
}

// joinLegacy sets the legacy network's code when none is set and starts it;
// the code already set opens it (created: false).
func (a *App) joinLegacy(w http.ResponseWriter, r *http.Request, code, addr, dir string) {
	a.mu.Lock()
	created, err := false, error(nil)
	switch {
	case a.s.Code == code:
	case a.s.Key() != nil:
		a.mu.Unlock()
		writeCodedError(w, http.StatusConflict, "legacy_exists")
		return
	default:
		created, err = true, a.startLegacyLocked(r.Context(), code, addr, dir)
	}
	v, _ := a.projectViewLocked(LegacyProjectID)
	a.mu.Unlock()
	if err != nil {
		a.failed(w, "join legacy network", err)
		return
	}
	if created {
		a.changed(LegacyProjectID, "status", "settings", "dashboard", "participants")
	}
	writeJSON(w, JoinResult{Project: v, Created: created})
}

func (a *App) startLegacyLocked(ctx context.Context, code, addr, dir string) error {
	s := a.s
	s.Code = code
	if dir != "" {
		s.WorkDir = dir
	}
	if addr != "" {
		s = s.WithPeer(addr)
	}
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s, a.configured = s, true
	a.syncHooksLocked()
	if err := a.ensureHubLocked(ctx); err != nil {
		return err
	}
	return a.startContextLocked(a.newLegacyContext(s.Key())) //nolint:contextcheck // runs under the Hub's context
}

func (a *App) renameProject(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	_, known := a.projectViewLocked(pid)
	c := a.projects[pid]
	a.mu.Unlock()
	switch {
	case !known:
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	case pid == LegacyProjectID:
		writeCodedError(w, http.StatusConflict, "legacy_rename")
		return
	}
	name, err := node.NormalizeProjectName(req.Name)
	if err != nil {
		writeCodedError(w, http.StatusBadRequest, "name")
		return
	}
	if c == nil {
		a.failed(w, "rename project", errors.New("project is not running"))
		return
	}
	if _, err := c.n.Rename(name); err != nil {
		a.failed(w, "rename project", err)
		return
	}
	a.changed(pid)
	a.writeView(w, pid)
}

// bindProject changes this member's alias, folder and auto-open of a project
// (absent: kept; "": cleared). A folder change needs an idle worker
// (project_busy).
func (a *App) bindProject(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req struct {
		Alias      *string `json:"alias"`
		Dir        *string `json:"dir"`
		AutoOpen   *bool   `json:"auto_open"`
		LaunchMode *string `json:"launch_mode"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	err := a.bindLocked(pid, req.Alias, req.Dir) //nolint:contextcheck // a folder change restarts the context under the Hub's context
	if err == nil && req.AutoOpen != nil {
		err = a.setAutoOpenLocked(pid, *req.AutoOpen)
	}
	if err == nil && req.LaunchMode != nil {
		err = a.setLaunchModeLocked(pid, *req.LaunchMode)
	}
	a.mu.Unlock()
	if err != nil {
		a.failed(w, "bind project", err)
		return
	}
	a.changed(pid, "settings", "status")
	a.writeView(w, pid)
}

func (a *App) bindLocked(pid string, alias, dir *string) error {
	if _, ok := a.projectViewLocked(pid); !ok {
		return ErrUnknownProject
	}
	legacy := pid == LegacyProjectID
	if alias != nil {
		*alias = strings.TrimSpace(*alias)
		if !settings.ValidAlias(*alias) || (legacy && *alias != "") {
			return &settings.Problem{Key: "alias"}
		}
	}
	if dir != nil {
		*dir = filepathClean(strings.TrimSpace(*dir))
		if err := a.checkDirLocked(pid, *dir); err != nil {
			return err
		}
		old := a.s.WorkDir
		if !legacy {
			old = a.s.Bindings[a.bindingIndex(pid)].Dir
		}
		if *dir != old {
			if err := a.setProjectDirLocked(pid, *dir); err != nil {
				return err
			}
		}
	}
	if alias == nil || legacy {
		return nil
	}
	i := a.bindingIndex(pid)
	if a.s.Bindings[i].Alias == *alias {
		return nil
	}
	s := a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i].Alias = *alias
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	return nil
}

// setAutoOpenLocked turns a project's auto-open on or off and applies it to
// its running node; the legacy network has none (bad_request).
func (a *App) setAutoOpenLocked(pid string, on bool) error {
	i := a.bindingIndex(pid)
	if i < 0 {
		return &settings.Problem{Key: "bad_request"}
	}
	if a.s.Bindings[i].AutoOpenOn() == on {
		return nil
	}
	s := a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i].AutoOpen = &on
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	if c := a.projects[pid]; c != nil {
		c.n.SetAutoOpen(on && s.Bindings[i].Dir != "")
	}
	return nil
}

// setLaunchModeLocked sets where a project opens sessions ("desktop" or
// "terminal") and applies it to its running node; the legacy network and
// other modes are bad_request.
func (a *App) setLaunchModeLocked(pid, mode string) error {
	i := a.bindingIndex(pid)
	if i < 0 || (mode != node.LaunchDesktop && mode != node.LaunchTerminal) {
		return &settings.Problem{Key: "bad_request"}
	}
	if a.s.Bindings[i].LaunchModeOf() == mode {
		return nil
	}
	s := a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i].LaunchMode = mode
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	if c := a.projects[pid]; c != nil {
		c.n.SetLaunchMode(mode)
	}
	return nil
}

// revealInvite answers the invite of a project, the only way its secret
// reaches the page; the legacy network's is its code.
func (a *App) revealInvite(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	a.mu.Lock()
	_, known := a.projectViewLocked(pid)
	s := a.s
	a.mu.Unlock()
	switch {
	case !known:
		writeCodedError(w, http.StatusNotFound, "not_found")
	case pid == LegacyProjectID && s.Code == "":
		writeCodedError(w, http.StatusConflict, "legacy_invite_unavailable")
	case pid == LegacyProjectID:
		writeJSON(w, InviteView{Invite: s.Code})
	default:
		b := s.Bindings[slices.IndexFunc(s.Bindings, func(b settings.ProjectBinding) bool { return b.ID == pid })]
		writeJSON(w, InviteView{Invite: config.FormatInvite(b.ID, b.Epoch, b.Secret)})
	}
}

// addProjectMember keeps a member's address in the binding (the legacy
// network's peers) and dials it.
func (a *App) addProjectMember(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req node.MemberRequest
	if !decode(w, r, &req) {
		return
	}
	addr, err := config.WithDefaultPort(strings.TrimSpace(req.Addr))
	a.mu.Lock()
	_, known := a.projectViewLocked(pid)
	a.mu.Unlock()
	switch {
	case !known:
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		writeCodedError(w, http.StatusBadRequest, "addr")
		return
	case pid == LegacyProjectID:
		err = a.AddMember(addr)
	default:
		err = a.addBindingPeer(pid, addr)
	}
	if err != nil {
		a.failed(w, "add member", err)
		return
	}
	a.writeView(w, pid)
}

func (a *App) addBindingPeer(pid, addr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.bindingIndex(pid)
	if i < 0 {
		return ErrUnknownProject
	}
	if !slices.Contains(a.s.Bindings[i].Peers, addr) {
		s := a.s
		s.Bindings = slices.Clone(s.Bindings)
		s.Bindings[i].Peers = append(slices.Clone(s.Bindings[i].Peers), addr)
		if err := settings.Save(a.path, s); err != nil {
			return err
		}
		a.s = s
	}
	if c := a.projects[pid]; c != nil {
		return c.n.AddPeer(addr)
	}
	return nil
}

// removeProjectMember removes a member from the project (the tombstone every
// member applies, node.RemoveMember) and drops its addresses from the binding
// (the legacy network's peers): body {"name"}.
func (a *App) removeProjectMember(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req node.MemberRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	a.mu.Lock()
	_, known := a.projectViewLocked(pid)
	a.mu.Unlock()
	var err error
	switch {
	case !known:
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	case name == "":
		writeCodedError(w, http.StatusBadRequest, "bad_request")
		return
	case pid == LegacyProjectID:
		err = a.RemoveMember(name)
	default:
		err = a.removeBindingMember(pid, name)
	}
	switch {
	case errors.Is(err, node.ErrSelf):
		writeCodedError(w, http.StatusBadRequest, "remove_self")
		return
	case errors.Is(err, node.ErrUnknownPeer):
		writeCodedError(w, http.StatusNotFound, "unknown_member")
		return
	case err != nil:
		a.failed(w, "remove member", err)
		return
	}
	a.changed(pid)
	a.writeView(w, pid)
}

func (a *App) removeBindingMember(pid, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.bindingIndex(pid)
	c := a.projects[pid]
	switch {
	case i < 0:
		return ErrUnknownProject
	case c == nil:
		return ErrNotRunning
	}
	var addrs []string
	for _, m := range c.n.Members() {
		if m.Name == name {
			addrs = m.Addrs
		}
	}
	// The settings are saved first: a failed save leaves the member in place,
	// a failed removal puts the saved settings back.
	peers := slices.DeleteFunc(slices.Clone(a.s.Bindings[i].Peers), func(p string) bool { return slices.Contains(addrs, p) })
	if len(peers) == len(a.s.Bindings[i].Peers) {
		return c.n.RemoveMember(name)
	}
	if len(peers) == 0 {
		peers = nil
	}
	old, s := a.s, a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i].Peers = peers
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	if err := c.n.RemoveMember(name); err != nil {
		if rerr := settings.Save(a.path, old); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	a.s = s
	return nil
}

func (a *App) leave(w http.ResponseWriter, r *http.Request) {
	if err := a.LeaveProject(r.PathValue("pid")); err != nil {
		a.failed(w, "leave project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// contextNode is the running node of pid; a missing one answers 404.
func (a *App) contextNode(w http.ResponseWriter, pid string) (*node.Node, bool) {
	a.mu.Lock()
	var c *appContext
	if pid == LegacyProjectID {
		c = a.legacy
	} else {
		c = a.projects[pid]
	}
	a.mu.Unlock()
	if c == nil {
		writeCodedError(w, http.StatusNotFound, "not_found")
		return nil, false
	}
	return c.n, true
}

func (a *App) projectChats(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	n, ok := a.contextNode(w, pid)
	if !ok {
		return
	}
	chats, err := n.Chats(r.URL.Query().Get("archive") == "1", pid == LegacyProjectID)
	if err != nil {
		a.failed(w, "project chats", err)
		return
	}
	out := make([]ChatInfoView, 0, len(chats))
	for _, c := range chats {
		out = append(out, ChatInfoView{ChatInfo: c, Project: pid})
	}
	writeJSON(w, out)
}

func (a *App) newProjectChat(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req struct {
		Participants []string `json:"participants"`
	}
	if !decode(w, r, &req) {
		return
	}
	n, ok := a.contextNode(w, pid)
	if !ok {
		return
	}
	var info node.ChatInfo
	var err error
	if pid == LegacyProjectID {
		info, err = n.CreateChat(req.Participants, "")
	} else {
		info, err = n.NewProjectChat(req.Participants)
	}
	if err != nil {
		a.chatFailed(w, err)
		return
	}
	writeJSON(w, ChatInfoView{ChatInfo: info, Project: pid})
}

// projectChat serves one chat of project pid: 404 unknown_chat for a chat
// that is not the project's.
func (a *App) projectChat(do func(n *node.Node, r *http.Request, id string) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid, id := r.PathValue("pid"), r.PathValue("id")
		n, ok := a.contextNode(w, pid)
		if !ok {
			return
		}
		if !n.OwnsChat(id) {
			writeCodedError(w, http.StatusNotFound, "unknown_chat")
			return
		}
		v, err := do(n, r, id)
		if err != nil {
			a.chatFailed(w, err)
			return
		}
		if info, ok := v.(node.ChatInfo); ok {
			v = ChatInfoView{ChatInfo: info, Project: pid}
		}
		writeJSON(w, v)
	}
}

func chatMessages(n *node.Node, r *http.Request, id string) (any, error) {
	var nums [3]uint64
	for i, k := range []string{"before", "after", "limit"} {
		if s := r.URL.Query().Get(k); s != "" {
			v, err := strconv.ParseUint(s, 10, 32)
			if err != nil {
				return nil, node.ErrBadRequest
			}
			nums[i] = v
		}
	}
	return n.ChatMessages(id, nums[0], nums[1], int(min(nums[2], 1000)))
}

// chatMembers adds and removes participants of a standalone project chat:
// body {"add": [names], "remove": [names]}.
func chatMembers(n *node.Node, r *http.Request, id string) (any, error) {
	var req struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody)).Decode(&req); err != nil {
		return nil, node.ErrBadRequest
	}
	return n.SetChatMembers(id, req.Add, req.Remove)
}

// projectSend sends into a chat of project pid, as a person: chat_id is
// required, and it and reply_to must be the project's.
func (a *App) projectSend(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	var req struct {
		ChatID      string            `json:"chat_id"`
		Body        string            `json:"body"`
		ReplyTo     string            `json:"reply_to"`
		Ask         []string          `json:"ask"`
		AskSeats    []string          `json:"ask_seats"`   // this node's local agents asked (seat ids)
		Attachments []node.Attachment `json:"attachments"` // uploaded: id and name
	}
	if !decode(w, r, &req) {
		return
	}
	n, ok := a.contextNode(w, pid)
	if !ok {
		return
	}
	req.ChatID, req.ReplyTo = strings.TrimSpace(req.ChatID), strings.TrimSpace(req.ReplyTo)
	if req.ChatID == "" || !n.OwnsChat(req.ChatID) || (req.ReplyTo != "" && !n.OwnsMessage(req.ReplyTo)) {
		writeCodedError(w, http.StatusNotFound, "unknown_chat")
		return
	}
	m, err := n.SendRequest(node.SendRequest{ChatID: req.ChatID, Body: req.Body, ReplyTo: req.ReplyTo, Ask: req.Ask,
		AuthorKind: node.AuthorHuman, Attachments: req.Attachments, AskSeats: req.AskSeats})
	if err != nil {
		if !attachmentFailed(w, err) {
			a.chatFailed(w, err)
		}
		return
	}
	writeJSON(w, m)
}

// chatFailed answers a node chat error with its code.
func (a *App) chatFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, node.ErrUnknownChat):
		writeCodedError(w, http.StatusNotFound, "unknown_chat")
	case errors.Is(err, node.ErrChatClosed):
		writeCodedError(w, http.StatusConflict, "chat_closed")
	case errors.Is(err, node.ErrLegacyChat):
		writeCodedError(w, http.StatusConflict, "chat_legacy")
	case errors.Is(err, node.ErrEmptyBody):
		writeCodedError(w, http.StatusBadRequest, "empty_body")
	case errors.Is(err, node.ErrBadParticipants), errors.Is(err, node.ErrUnknownPeer), errors.Is(err, node.ErrNoChatSupport):
		writeCodedError(w, http.StatusBadRequest, "chat_participants")
	case errors.Is(err, node.ErrBadRequest), errors.Is(err, node.ErrNotProject):
		writeCodedError(w, http.StatusBadRequest, "bad_request")
	case errors.Is(err, node.ErrNotChatOwner):
		writeCodedError(w, http.StatusForbidden, "chat_owner")
	default:
		a.log.Error("chat request", "err", err)
		writeCodedError(w, http.StatusInternalServerError, "internal")
	}
}
