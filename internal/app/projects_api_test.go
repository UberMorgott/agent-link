package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// projectsHarness is a configured app without a legacy network, listening
// on listen ("" = any loopback port).
func projectsHarness(t *testing.T, name, listen string, setup ...func(*App)) *harness {
	t.Helper()
	h := newHarness(t, append([]func(*App){func(a *App) { a.s.Code = "" }}, setup...)...)
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	body := jsonOf(t, map[string]any{"node": name, "listen": listen, "handler": "none"})
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", body, h.tokenHdr()); code != http.StatusOK || strings.Contains(raw, `"error"`) {
		t.Fatalf("save: %d %s", code, raw)
	}
	return h
}

// api calls the projects API and decodes a 200 answer into out (nil: none).
func (h *harness) api(t *testing.T, method, path string, body any, out any) (int, string) {
	t.Helper()
	raw := ""
	if body != nil {
		raw = jsonOf(t, body)
	}
	code, got := h.do(t, method, "/ui/api/"+path, raw, h.tokenHdr())
	if code == http.StatusOK && out != nil {
		if err := json.Unmarshal([]byte(got), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, got)
		}
	}
	return code, got
}

// wantError checks a projects API error: status, code and its sentence.
func (h *harness) wantError(t *testing.T, method, path string, body any, status int, code string) {
	t.Helper()
	got, raw := h.api(t, method, path, body, nil)
	var e apiError
	if err := json.Unmarshal([]byte(raw), &e); err != nil || got != status || e.Code != code || e.Error != uiStrings["error."+code] {
		t.Errorf("%s %s: %d %s, want %d %s", method, path, got, raw, status, code)
	}
}

// Every row of §7.2 on one app: create, list, read, rename, bind, reveal,
// members, join, leave and the chats of a project, with their errors.
func TestProjectsAPI(t *testing.T) {
	h := projectsHarness(t, "alice", "")
	dir, dir2 := t.TempDir(), t.TempDir()
	sub := h.app.events.subscribe()
	defer sub.close()
	<-sub.wake
	sub.snapshot()

	var list []ProjectView
	if code, raw := h.api(t, http.MethodGet, "projects", nil, &list); code != http.StatusOK || len(list) != 0 {
		t.Fatalf("empty list: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, "projects", map[string]any{"name": " "}, http.StatusBadRequest, "name")
	h.wantError(t, http.MethodPost, "projects", map[string]any{"name": "x", "alias": strings.Repeat("я", 65)}, http.StatusBadRequest, "alias")
	h.wantError(t, http.MethodPost, "projects", map[string]any{"name": "x", "dir": "relative"}, http.StatusBadRequest, "dir")
	h.wantError(t, http.MethodPost, "projects", "not an object", http.StatusBadRequest, "bad_request")

	var site, bare ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": " Сайт ", "dir": dir}, &site); code != http.StatusOK ||
		site.Name != "Сайт" || site.Display != "Сайт" || site.State != ProjectReady || !site.CanRename || !site.HasInvite ||
		site.Legacy || len(site.Members) != 1 || !site.Members[0].Self {
		t.Fatalf("create: %d %s", code, raw)
	}
	waitTopic(t, sub, "project:"+site.ID)
	h.wantError(t, http.MethodPost, "projects", map[string]any{"name": "x", "dir": dir}, http.StatusBadRequest, "dir_taken")
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Архив"}, &bare); code != http.StatusOK || bare.State != ProjectNeedsFolder {
		t.Fatalf("create without a folder: %d %s", code, raw)
	}
	secret := h.app.Settings().Bindings[0].Secret
	if code, raw := h.api(t, http.MethodGet, "projects", nil, &list); code != http.StatusOK || len(list) != 2 ||
		list[0].ID != bare.ID || list[1].ID != site.ID || strings.Contains(raw, secret) {
		t.Fatalf("list: %d %s", code, raw)
	}

	// Read, rename.
	h.wantError(t, http.MethodGet, "projects/"+strings.Repeat("A", 26), nil, http.StatusNotFound, "not_found")
	var v ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/name", map[string]any{"name": "Сайт 2"}, &v); code != http.StatusOK || v.Name != "Сайт 2" {
		t.Fatalf("rename: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/name", map[string]any{"name": ""}, http.StatusBadRequest, "name")
	h.wantError(t, http.MethodPost, "projects/"+strings.Repeat("A", 26)+"/name", map[string]any{"name": "x"}, http.StatusNotFound, "not_found")

	// The invite is the only way to the secret.
	var inv InviteView
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/invite", nil, &inv); code != http.StatusOK {
		t.Fatalf("invite: %d %s", code, raw)
	}
	if got, err := config.ParseInvite(inv.Invite); err != nil || got.ProjectID != site.ID || got.Secret != secret {
		t.Fatalf("invite %q: %+v %v", inv.Invite, got, err)
	}
	if _, raw := h.api(t, http.MethodGet, "projects/"+site.ID, nil, nil); strings.Contains(raw, secret) {
		t.Fatal("the view carries the secret")
	}

	// Binding: alias, folder cleared and bound again, errors.
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"alias": "Мой"}, &v); code != http.StatusOK ||
		v.Display != "Мой" || v.Dir != dir {
		t.Fatalf("alias: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"dir": ""}, &v); code != http.StatusOK ||
		v.State != ProjectNeedsFolder || v.Alias != "Мой" {
		t.Fatalf("clear folder: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"dir": dir2}, &v); code != http.StatusOK || v.State != ProjectReady {
		t.Fatalf("bind folder: %d %s", code, raw)
	}
	// Auto-open: on by default, off and on again, kept in the settings.
	if !v.AutoOpen {
		t.Fatalf("auto-open not on by default: %+v", v)
	}
	v = ProjectView{}
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"auto_open": false}, &v); code != http.StatusOK ||
		v.AutoOpen || h.app.Settings().Bindings[h.app.bindingIndex(site.ID)].AutoOpenOn() || h.app.projects[site.ID].n.AutoOpen() {
		t.Fatalf("auto-open off: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"auto_open": true}, &v); code != http.StatusOK ||
		!v.AutoOpen || !h.app.projects[site.ID].n.AutoOpen() {
		t.Fatalf("auto-open on: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, "projects/"+bare.ID+"/binding", map[string]any{"dir": dir2}, http.StatusBadRequest, "dir_taken")
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"dir": "relative"}, http.StatusBadRequest, "dir")
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"alias": "a\x01"}, http.StatusBadRequest, "alias")
	h.wantError(t, http.MethodPost, "projects/"+strings.Repeat("A", 26)+"/binding", map[string]any{}, http.StatusNotFound, "not_found")

	// Members.
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/members/add", map[string]any{"addr": "10.0.0.1:99999"}, http.StatusBadRequest, "addr")
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/members/add", map[string]any{"addr": "127.0.0.1"}, &v); code != http.StatusOK {
		t.Fatalf("add member: %d %s", code, raw)
	}
	if got := h.app.Settings().Bindings[0].Peers; !slices.Equal(got, []string{"127.0.0.1:7420"}) {
		t.Fatalf("binding peers %v", got)
	}

	// Join: bad input, a known invite, another secret for the same project.
	h.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": "ALP1.nope"}, http.StatusBadRequest, "invite")
	h.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": inv.Invite, "addr": "a b"}, http.StatusBadRequest, "addr")
	var jr JoinResult
	if code, raw := h.api(t, http.MethodPost, "projects/join", map[string]any{"invite": strings.ToLower(inv.Invite)}, &jr); code != http.StatusOK ||
		jr.Created || jr.Project.ID != site.ID {
		t.Fatalf("known invite: %d %s", code, raw)
	}
	other, _ := config.NewProjectSecret()
	h.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": config.FormatInvite(site.ID, config.ProjectEpoch, other)},
		http.StatusConflict, "conflict_secret")

	// The legacy network: joined by its code, listed last, not renamed.
	if code, raw := h.api(t, http.MethodPost, "projects/join", map[string]any{"invite": "k7q2-mxpa-4rtb"}, &jr); code != http.StatusOK ||
		!jr.Created || !jr.Project.Legacy || jr.Project.ID != LegacyProjectID || jr.Project.Name != legacyName {
		t.Fatalf("join legacy: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/join", map[string]any{"invite": "K7Q2-MXPA-4RTB"}, &jr); code != http.StatusOK || jr.Created {
		t.Fatalf("join legacy again: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": "AAAA-BBBB-CCCC"}, http.StatusConflict, "legacy_exists")
	h.wantError(t, http.MethodPost, "projects/legacy/name", map[string]any{"name": "x"}, http.StatusConflict, "legacy_rename")
	if code, raw := h.api(t, http.MethodPost, "projects/legacy/invite", nil, &inv); code != http.StatusOK || inv.Invite != "K7Q2-MXPA-4RTB" {
		t.Fatalf("legacy invite: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodGet, "projects", nil, &list); code != http.StatusOK || len(list) != 3 || !list[2].Legacy {
		t.Fatalf("list with legacy: %d %s", code, raw)
	}
	// The settings page neither reads nor changes the code.
	if _, raw := h.api(t, http.MethodGet, "settings", nil, nil); strings.Contains(raw, "K7Q2") {
		t.Fatalf("settings carry the code: %s", raw)
	}
	if code, raw := h.api(t, http.MethodPost, "settings", map[string]any{"node": "alice", "code": "ZZZZ-ZZZZ-ZZZZ", "listen": "127.0.0.1:0", "handler": "none"}, nil); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, raw)
	}
	if s := h.app.Settings(); s.Code != "K7Q2-MXPA-4RTB" || len(s.Bindings) != 2 {
		t.Fatalf("a settings save changed the code or bindings: %q %d", s.Code, len(s.Bindings))
	}

	// Chats of a project.
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/chats", map[string]any{"participants": []string{"bob"}}, http.StatusBadRequest, "chat_participants")
	var chats []ChatInfoView
	if code, raw := h.api(t, http.MethodGet, "projects/"+site.ID+"/chats?archive=1", nil, &chats); code != http.StatusOK || len(chats) != 0 {
		t.Fatalf("chats: %d %s", code, raw)
	}
	unknown := strings.Repeat("ab", 16)
	h.wantError(t, http.MethodGet, "projects/"+site.ID+"/chats/"+unknown, nil, http.StatusNotFound, "unknown_chat")
	h.wantError(t, http.MethodGet, "projects/"+site.ID+"/chats/"+unknown+"/messages", nil, http.StatusNotFound, "unknown_chat")
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/chats/"+unknown+"/close", nil, http.StatusNotFound, "unknown_chat")
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/chats/"+unknown+"/members", map[string]any{"add": []string{"bob"}}, http.StatusNotFound, "unknown_chat")
	// A chat of this node alone; bob is no member to invite.
	var solo ChatInfoView
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/chats", map[string]any{"participants": []string{}}, &solo); code != http.StatusOK ||
		len(solo.Participants) != 1 || solo.Owner != solo.Participants[0] || solo.Archived {
		t.Fatalf("solo chat: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/chats/"+solo.ID+"/members", map[string]any{"add": []string{"bob"}}, http.StatusBadRequest, "chat_participants")
	h.wantError(t, http.MethodPost, "projects/"+site.ID+"/send", map[string]any{"body": "hi"}, http.StatusNotFound, "unknown_chat")
	h.wantError(t, http.MethodGet, "projects/"+strings.Repeat("A", 26)+"/chats", nil, http.StatusNotFound, "not_found")
	if code, raw := h.api(t, http.MethodGet, "sessions", nil, nil); code != http.StatusOK || strings.TrimSpace(raw) != "[]" {
		t.Fatalf("sessions: %d %s", code, raw)
	}

	// Leave.
	if code, raw := h.api(t, http.MethodPost, "projects/"+bare.ID+"/leave", nil, nil); code != http.StatusNoContent {
		t.Fatalf("leave: %d %s", code, raw)
	}
	h.wantError(t, http.MethodGet, "projects/"+bare.ID, nil, http.StatusNotFound, "not_found")
	h.wantError(t, http.MethodPost, "projects/"+bare.ID+"/leave", nil, http.StatusNotFound, "not_found")
	if code, _ := h.api(t, http.MethodPost, "projects/legacy/leave", nil, nil); code != http.StatusNoContent || h.app.Settings().Code != "" {
		t.Fatalf("leave legacy: %d", code)
	}

	// The shell answers the project routes.
	for _, path := range []string{"/ui/welcome", "/ui/p/" + site.ID, "/ui/p/" + site.ID + "/c/" + unknown} {
		if code, body := h.do(t, http.MethodGet, path, "", nil); code != http.StatusOK || !strings.Contains(body, `<div id="app">`) {
			t.Errorf("%s: %d", path, code)
		}
	}
}

// A busy worker refuses a folder change and a leave; the legacy network with
// a long secret has no invite; the 33rd project is refused.
func TestProjectsAPIRefusals(t *testing.T) {
	on := true
	h := projectsHarness(t, "alice", "", func(a *App) { a.s.HandlerCommand = []string{"agentlink-test-no-such-agent"} })
	if code, raw := h.api(t, http.MethodPost, "settings", map[string]any{"node": "alice", "listen": "127.0.0.1:0", "handler": "claude", "auto_answer": on}, nil); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, raw)
	}
	var p ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Сайт", "dir": t.TempDir()}, &p); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	h.app.mu.Lock()
	c, slots := h.app.projects[p.ID], h.app.slots
	h.app.mu.Unlock()
	for range worker.DefaultMaxJobs {
		defer slots.Hold()()
	}
	const id = "0123456789abcdef0123456789abcdef"
	if err := c.w.Accept(node.Message{ID: id, From: "bob", To: "alice", Body: "hi", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var v ProjectView
	if code, raw := h.api(t, http.MethodGet, "projects/"+p.ID, nil, &v); code != http.StatusOK || !v.Busy {
		t.Fatalf("busy view: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, "projects/"+p.ID+"/binding", map[string]any{"dir": t.TempDir()}, http.StatusConflict, "project_busy")
	h.wantError(t, http.MethodPost, "projects/"+p.ID+"/leave", nil, http.StatusConflict, "project_busy")
	c.w.Cancel(id)

	long := newHarness(t, func(a *App) { a.s.Code, a.s.Secret = "", strings.Repeat("s", 32) })
	long.wantError(t, http.MethodPost, "projects/legacy/invite", nil, http.StatusConflict, "legacy_invite_unavailable")

	full := newHarness(t, func(a *App) {
		a.s.Code = ""
		for range settings.MaxProjects {
			a.s.Bindings = append(a.s.Bindings, newBinding(t, ""))
		}
	})
	full.wantError(t, http.MethodPost, "projects", map[string]any{"name": "x"}, http.StatusConflict, "too_many_projects")
	id2, _ := config.NewProjectID()
	secret, _ := config.NewProjectSecret()
	full.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": config.FormatInvite(id2, config.ProjectEpoch, secret)},
		http.StatusConflict, "too_many_projects")
}

// Two apps on loopback through the web API: create, invite, join with the
// address, the name arrives, bind a folder, a new chat, messages both ways.
// The project counts leave out this node wherever the list puts it, and
// count every other member once.
func TestPeerCounts(t *testing.T) {
	for _, tc := range []struct {
		members       []node.MemberInfo
		online, total int
	}{
		{[]node.MemberInfo{{Name: "me", Self: true, Online: true}}, 0, 0},
		{[]node.MemberInfo{{Name: "me", Self: true, Online: true}, {Name: "KPECTIK", Online: true}}, 1, 1},
		{[]node.MemberInfo{{Name: "me", Self: true, Online: true}, {Name: "bob", Online: true}, {Name: "carl"}}, 1, 2},
		{[]node.MemberInfo{{Name: "bob", Online: true}, {Name: "me", Self: true, Online: true}}, 1, 1},
	} {
		if online, total := peerCounts(tc.members); online != tc.online || total != tc.total {
			t.Errorf("%+v: online=%d total=%d, want %d/%d", tc.members, online, total, tc.online, tc.total)
		}
	}
}

func TestProjectsJoinFlow(t *testing.T) {
	addr := freeAddr(t)
	alice := projectsHarness(t, "alice", addr)
	bob := projectsHarness(t, "bob", "")
	var p ProjectView
	if code, raw := alice.api(t, http.MethodPost, "projects", map[string]any{"name": "Сайт", "dir": t.TempDir()}, &p); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	var inv InviteView
	alice.api(t, http.MethodPost, "projects/"+p.ID+"/invite", nil, &inv)
	var jr JoinResult
	if code, raw := bob.api(t, http.MethodPost, "projects/join", map[string]any{"invite": inv.Invite, "addr": addr}, &jr); code != http.StatusOK ||
		!jr.Created || jr.Project.ID != p.ID {
		t.Fatalf("join: %d %s", code, raw)
	}
	var v ProjectView
	eventuallyLong(t, "the name reaches bob", func() bool {
		bob.api(t, http.MethodGet, "projects/"+p.ID, nil, &v)
		return v.Name == "Сайт" && v.State == ProjectNeedsFolder && v.Online == 1
	})
	// Two members, both online (bob himself and alice): the counts are of the
	// other members, the list has this node too.
	if v.Total != 1 || len(v.Members) != 2 || !v.Members[0].Self || !v.Members[1].Online {
		t.Fatalf("counts online=%d total=%d, members %+v", v.Online, v.Total, v.Members)
	}
	if code, raw := bob.api(t, http.MethodPost, "projects/"+p.ID+"/binding", map[string]any{"dir": t.TempDir()}, &v); code != http.StatusOK || v.State != ProjectReady {
		t.Fatalf("bind: %d %s", code, raw)
	}
	eventuallyLong(t, "bob reconnected", func() bool {
		bob.api(t, http.MethodGet, "projects/"+p.ID, nil, &v)
		return v.Online == 1
	})
	var chat ChatInfoView
	if code, raw := bob.api(t, http.MethodPost, "projects/"+p.ID+"/chats", map[string]any{"participants": []string{"alice"}}, &chat); code != http.StatusOK ||
		chat.Mode != node.ChatModeProject || chat.Project != p.ID {
		t.Fatalf("new chat: %d %s", code, raw)
	}
	var sent node.Message
	if code, raw := bob.api(t, http.MethodPost, "projects/"+p.ID+"/send", map[string]any{"chat_id": chat.ID, "body": "привет", "ask": []string{"alice"}}, &sent); code != http.StatusOK ||
		sent.AuthorKind != node.AuthorHuman {
		t.Fatalf("send: %d %s", code, raw)
	}
	var msgs []node.ChatMessage
	eventuallyLong(t, "alice got bob's message", func() bool {
		code, _ := alice.api(t, http.MethodGet, "projects/"+p.ID+"/chats/"+chat.ID+"/messages", nil, &msgs)
		return code == http.StatusOK && len(msgs) > 0 && msgs[len(msgs)-1].ID == sent.ID
	})
	if code, raw := alice.api(t, http.MethodPost, "projects/"+p.ID+"/send", map[string]any{"chat_id": chat.ID, "body": "ответ", "reply_to": sent.ID}, &sent); code != http.StatusOK {
		t.Fatalf("reply: %d %s", code, raw)
	}
	eventuallyLong(t, "bob got the reply", func() bool {
		code, _ := bob.api(t, http.MethodGet, "projects/"+p.ID+"/chats/"+chat.ID+"/messages", nil, &msgs)
		return code == http.StatusOK && slices.ContainsFunc(msgs, func(m node.ChatMessage) bool { return m.ID == sent.ID })
	})
	alice.wantError(t, http.MethodPost, "projects/legacy/chats/"+chat.ID+"/close", nil, http.StatusNotFound, "not_found")
	if code, raw := alice.api(t, http.MethodPost, "projects/"+p.ID+"/name", map[string]any{"name": "Сайт 2"}, nil); code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, raw)
	}
	eventuallyLong(t, "the new name reaches bob", func() bool {
		bob.api(t, http.MethodGet, "projects/"+p.ID, nil, &v)
		return v.Name == "Сайт 2"
	})
	var closed ChatInfoView
	if code, raw := alice.api(t, http.MethodPost, "projects/"+p.ID+"/chats/"+chat.ID+"/close", nil, &closed); code != http.StatusOK || !closed.Closed {
		t.Fatalf("close: %d %s", code, raw)
	}
	alice.wantError(t, http.MethodPost, "projects/"+p.ID+"/send", map[string]any{"chat_id": chat.ID, "body": "ещё"}, http.StatusConflict, "chat_closed")
}

// Joining the legacy network while an agent answers needs its working folder:
// without one the join answers 400 work_dir, with dir it is saved.
func TestJoinLegacyNeedsWorkDir(t *testing.T) {
	h := projectsHarness(t, "alice", "")
	h.app.mu.Lock()
	h.app.s.Handler, h.app.s.HandlerCommand, h.app.s.WorkDir = "claude", []string{"claude"}, ""
	h.app.mu.Unlock()
	h.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": "K7Q2-MXPA-4RTB"}, http.StatusBadRequest, "work_dir")
	h.wantError(t, http.MethodPost, "projects/join", map[string]any{"invite": "K7Q2-MXPA-4RTB", "dir": "relative"}, http.StatusBadRequest, "work_dir")
	dir := t.TempDir()
	var jr JoinResult
	if code, raw := h.api(t, http.MethodPost, "projects/join", map[string]any{"invite": "K7Q2-MXPA-4RTB", "dir": dir}, &jr); code != http.StatusOK ||
		!jr.Created || !jr.Project.Legacy || jr.Project.Dir != dir {
		t.Fatalf("join with dir: %d %s", code, raw)
	}
	if got := h.app.Settings().WorkDir; got != dir {
		t.Fatalf("work_dir %q, want %q", got, dir)
	}
}
