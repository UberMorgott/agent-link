package app

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/UberMorgott/agent-link/internal/node"
)

// The loopback control API (agents, the CLI and the folder hooks) serves
// every context (docs/plans/projects-v1.md §6.1). Each request goes to one
// context's node, picked by the selector `project` (query parameter or JSON
// field: a project id or LegacyProjectID), else by the chat or message it
// names (ids are project-scoped), else by its folder (the deepest bound
// project folder that holds it; `cwd`, the caller's working folder, is a
// routing hint only), else the legacy network, else the only project when the
// request names no folder at all. An explicit
// selector never falls back: an unknown one is 404, and the context must hold
// every chat and message the request names (409). A request no context takes
// is 400 naming the known projects. Lists without a selector (chats,
// sessions) cover every context and name each item's project.

// routeCtx is one running context as the router sees it.
type routeCtx struct {
	id  string // project id or LegacyProjectID
	n   *node.Node
	dir string // bound folder of a project; "" for the legacy network or none
}

// routeError is a request the router refuses, as a plain-text control API error.
type routeError struct {
	status int
	text   string
}

func (e *routeError) write(w http.ResponseWriter) { http.Error(w, e.text, e.status) }

var (
	errAmbiguousOwner = &routeError{http.StatusConflict, "ambiguous: the id is in more than one project; name the project"}
	errNotInProject   = &routeError{http.StatusConflict, "the chat or message is not in that project"}
	errNeedsFolder    = &routeError{http.StatusConflict, "project_needs_folder: " + node.ErrNeedsFolder.Error()}
	errFolderOutside  = &routeError{http.StatusConflict, "folder_not_in_project: the folder is not inside the project's folder"}
)

// routeContexts lists the running contexts, the legacy network first.
func (a *App) routeContexts() []routeCtx {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []routeCtx
	if a.legacy != nil {
		out = append(out, routeCtx{id: LegacyProjectID, n: a.legacy.n})
	}
	for _, b := range a.s.Bindings {
		if c := a.projects[b.ID]; c != nil {
			out = append(out, routeCtx{id: b.ID, n: c.n, dir: b.Dir})
		}
	}
	return out
}

// selector is what a control API request names to pick its context.
type selector struct {
	project string
	chat    string   // chat id: its owner
	ids     []string // message ids in rule order (reply_to, then parent): their owner
	folder  string
	cwd     string // the caller's working folder: picks a project, filters nothing
}

// noContext is the error of a request no context takes: it names the
// selector that would, with the known projects.
func noContext(ctxs []routeCtx) *routeError {
	ids := make([]string, 0, len(ctxs))
	for _, c := range ctxs {
		ids = append(ids, c.id)
	}
	hint := "no project is joined yet"
	if len(ids) > 0 {
		hint = "pass --project <id>; known: " + strings.Join(ids, ", ")
	}
	return &routeError{http.StatusBadRequest, "folder is not in a project: " + hint}
}

// route picks the context of sel.
func route(ctxs []routeCtx, sel selector) (routeCtx, *routeError) {
	if sel.project != "" {
		i := slices.IndexFunc(ctxs, func(c routeCtx) bool { return c.id == sel.project })
		if i < 0 {
			return routeCtx{}, &routeError{http.StatusNotFound, "unknown project " + sel.project}
		}
		c := ctxs[i]
		if sel.chat != "" && !c.n.OwnsChat(sel.chat) {
			return routeCtx{}, errNotInProject
		}
		for _, id := range sel.ids {
			if id != "" && !c.n.OwnsMessage(id) {
				return routeCtx{}, errNotInProject
			}
		}
		return c, nil
	}
	if sel.chat != "" {
		if c, ok, err := owner(ctxs, func(n *node.Node) bool { return n.OwnsChat(sel.chat) }); ok || err != nil {
			return c, err
		}
	}
	for _, id := range sel.ids {
		if id == "" {
			continue
		}
		if c, ok, err := owner(ctxs, func(n *node.Node) bool { return n.OwnsMessage(id) }); ok || err != nil {
			return c, err
		}
	}
	for _, folder := range []string{sel.folder, sel.cwd} {
		if folder == "" {
			continue
		}
		best := -1
		for i, c := range ctxs {
			if c.dir != "" && within(c.dir, folder) && (best < 0 || len(filepath.Clean(c.dir)) > len(filepath.Clean(ctxs[best].dir))) {
				best = i
			}
		}
		if best >= 0 {
			return ctxs[best], nil
		}
	}
	if len(ctxs) > 0 && ctxs[0].id == LegacyProjectID {
		return ctxs[0], nil
	}
	// Without the legacy network a request that names no folder at all is the
	// only project's; a folder outside it is refused rather than guessed.
	if len(ctxs) == 1 && sel.folder == "" && sel.cwd == "" {
		return ctxs[0], nil
	}
	return routeCtx{}, noContext(ctxs)
}

// owner is the one context for which has holds; ok is false for none.
func owner(ctxs []routeCtx, has func(*node.Node) bool) (routeCtx, bool, *routeError) {
	var found []routeCtx
	for _, c := range ctxs {
		if has(c.n) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return routeCtx{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return routeCtx{}, false, errAmbiguousOwner
}

// within reports whether path is dir or inside it (case-insensitively on
// Windows, like filepath.Rel).
func within(dir, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// folderScoped checks a context for the endpoints of agent sessions
// (sessions, wait, unread): a project without a folder has none, and a given
// folder must be inside the project's.
func folderScoped(c routeCtx, folder string) *routeError {
	if c.id == LegacyProjectID {
		return nil
	}
	if c.n.NeedsFolder() {
		return errNeedsFolder
	}
	if folder != "" && !within(c.dir, folder) {
		return errFolderOutside
	}
	return nil
}

// controlAPI serves the control API for every context.
func (a *App) controlAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Project string `json:"project"`
			ChatID  string `json:"chat_id"`
			ReplyTo string `json:"reply_to"`
			Parent  string `json:"parent"`
			Folder  string `json:"folder"`
		}
		if !peekJSON(w, r, &body) {
			return
		}
		a.forward(w, r, selector{project: pick(r, body.Project), chat: body.ChatID, ids: []string{body.ReplyTo, body.Parent}, folder: body.Folder}, false)
	})
	mux.HandleFunc("GET /wait", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a.forward(w, r, selector{project: q.Get("project"), chat: q.Get("chat"), folder: q.Get("folder")}, true)
	})
	mux.HandleFunc("GET /unread", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a.forward(w, r, selector{project: q.Get("project"), folder: q.Get("folder"), cwd: q.Get("cwd")}, true)
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Project string `json:"project"`
			Folder  string `json:"folder"`
		}
		if peekJSON(w, r, &body) {
			a.forward(w, r, selector{project: pick(r, body.Project), folder: body.Folder}, true)
		}
	})
	mux.HandleFunc("POST /claim", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Project string `json:"project"`
			Folder  string `json:"folder"`
		}
		if peekJSON(w, r, &body) {
			a.forward(w, r, selector{project: pick(r, body.Project), folder: body.Folder}, true)
		}
	})
	mux.HandleFunc("GET /sessions", a.controlSessions)
	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, c := range a.routeContexts() {
			if slices.ContainsFunc(c.n.Sessions(), func(s node.Session) bool { return s.SessionID == id }) {
				c.n.APIHandler().ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, node.ErrUnknownSession.Error()+" "+id, http.StatusNotFound)
	})
	mux.HandleFunc("POST /ack", a.controlAck)
	byChat := func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ctxs := a.routeContexts()
		if project := r.URL.Query().Get("project"); project == "" &&
			!slices.ContainsFunc(ctxs, func(c routeCtx) bool { return c.n.OwnsChat(id) }) {
			http.Error(w, node.ErrUnknownChat.Error()+" "+id, http.StatusNotFound)
			return
		}
		c, err := route(ctxs, selector{project: r.URL.Query().Get("project"), chat: id})
		if err != nil {
			err.write(w)
			return
		}
		c.n.APIHandler().ServeHTTP(w, r)
	}
	for _, p := range []string{"POST /chats/{id}/ack", "POST /chats/{id}/activity", "GET /chats/{id}", "GET /chats/{id}/messages"} {
		mux.HandleFunc(p, byChat)
	}
	mux.HandleFunc("GET /chats", a.controlChats)
	mux.HandleFunc("GET /projects", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.Projects()) })
	// The rest names only a project: that context, else the one of the
	// caller's folder, else the legacy network (route).
	byProject := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a.forward(w, r, selector{project: q.Get("project"), cwd: q.Get("cwd")}, false)
	}
	for _, p := range []string{"POST /chats", "GET /inbox", "GET /members", "POST /members", "POST /members/remove"} {
		mux.HandleFunc(p, byProject)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" { // browsers never reach the control API
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// pick is the selector of the query, else of the JSON body.
func pick(r *http.Request, fromBody string) string {
	return cmp.Or(r.URL.Query().Get("project"), fromBody)
}

// peekJSON decodes the body into v and leaves it readable again for the node.
func peekJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err == nil {
		err = json.Unmarshal(data, v)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}

// forward hands the request to the node of the context sel picks; scoped
// marks the endpoints of agent sessions (folderScoped).
func (a *App) forward(w http.ResponseWriter, r *http.Request, sel selector, scoped bool) {
	c, err := route(a.routeContexts(), sel)
	if err == nil && scoped {
		err = folderScoped(c, sel.folder)
	}
	if err != nil {
		err.write(w)
		return
	}
	c.n.APIHandler().ServeHTTP(w, r)
}

// controlChats serves GET /chats: one context's with a selector, else every
// context's, each naming its project, most recent first.
func (a *App) controlChats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("project") != "" {
		a.forward(w, r, selector{project: q.Get("project")}, false)
		return
	}
	out := []ChatInfoView{}
	for _, c := range a.routeContexts() {
		chats, err := c.n.Chats(q.Get("archive") == "1", q.Get("legacy") == "1")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, ci := range chats {
			out = append(out, ChatInfoView{ChatInfo: ci, Project: c.id})
		}
	}
	slices.SortStableFunc(out, func(x, y ChatInfoView) int { return y.LastAt.Compare(x.LastAt) })
	writeJSON(w, out)
}

// controlSessions serves GET /sessions: every context's live sessions, each
// naming its project.
func (a *App) controlSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.allSessions())
}

// allSessions lists every context's live sessions with their project.
func (a *App) allSessions() []SessionView {
	out := []SessionView{}
	for _, c := range a.routeContexts() {
		for _, s := range c.n.Sessions() {
			out = append(out, SessionView{Session: s, Project: c.id})
		}
	}
	return out
}

// controlAck serves POST /ack: with a selector that context acks every id;
// without one each id goes to the context that holds it, and ids none holds
// are not found.
func (a *App) controlAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		node.AckRequest
		Project string `json:"project"`
	}
	if !peekJSON(w, r, &req) {
		return
	}
	if project := pick(r, req.Project); project != "" {
		a.forward(w, r, selector{project: project}, false)
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 1000 {
		http.Error(w, "ids required (at most 1000)", http.StatusBadRequest)
		return
	}
	ctxs := a.routeContexts()
	groups := map[string][]string{}
	for _, id := range req.IDs {
		c, ok, err := owner(ctxs, func(n *node.Node) bool { return n.OwnsMessage(id) })
		if err != nil {
			err.write(w)
			return
		}
		if ok {
			groups[c.id] = append(groups[c.id], id)
		}
	}
	results := map[string]node.AckResult{}
	for _, c := range ctxs {
		ids := groups[c.id]
		if len(ids) == 0 {
			continue
		}
		res, err := c.n.Ack("", node.AckRequest{IDs: ids, SessionID: req.SessionID})
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, node.ErrBadRequest) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		for _, x := range res {
			results[x.ID] = x
		}
	}
	out := make([]node.AckResult, 0, len(req.IDs))
	for _, id := range req.IDs {
		x, ok := results[id]
		if !ok {
			x = node.AckResult{ID: id}
		}
		out = append(out, x)
	}
	writeJSON(w, out)
}
