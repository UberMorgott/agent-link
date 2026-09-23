package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
)

// contractDir holds the projects API contract (docs/plans/projects-v1.md §7):
// one JSON file per DTO and per error, as served. The web UI's tests load the
// same files for their mocks.
var contractDir = filepath.Join("testdata", "projects")

// contractErrors are the error codes of the projects API with their status.
var contractErrors = map[string]int{
	"name": http.StatusBadRequest, "alias": http.StatusBadRequest, "dir": http.StatusBadRequest,
	"dir_taken": http.StatusBadRequest, "invite": http.StatusBadRequest, "addr": http.StatusBadRequest,
	"chat_participants": http.StatusBadRequest, "empty_body": http.StatusBadRequest, "bad_request": http.StatusBadRequest,
	"too_many_projects": http.StatusConflict, "conflict_secret": http.StatusConflict, "legacy_exists": http.StatusConflict,
	"legacy_rename": http.StatusConflict, "project_busy": http.StatusConflict, "legacy_invite_unavailable": http.StatusConflict,
	"chat_legacy": http.StatusConflict, "chat_closed": http.StatusConflict,
	"project_needs_folder": http.StatusConflict, "folder_not_in_project": http.StatusConflict,
	"not_found": http.StatusNotFound, "unknown_chat": http.StatusNotFound,
}

// contractValues builds every DTO from a fixed state.
func contractValues() map[string]any {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	const pid = "MFRGGZDFMZTWQ2LKNNWG23TPOA"
	self := node.MemberInfo{Name: "alice", Self: true, Online: true, Addrs: []string{"10.147.20.5:7420"}, App: "v0.9.0", Proto: 6}
	bob := node.MemberInfo{Name: "bob", Online: true, Addrs: []string{"10.147.20.9:7420"}, Seen: at, App: "v0.9.0", Proto: 6}
	carol := node.MemberInfo{Name: "carol", Addrs: []string{"10.147.20.11:7420"}, Seen: at.Add(-time.Hour), App: "v0.9.0", Proto: 6}
	ready := ProjectView{ID: pid, Name: "Сайт", Alias: "", Display: "Сайт", Dir: `C:\work\site`, State: ProjectReady,
		Online: 1, Total: 2, Members: []node.MemberInfo{self, bob, carol}, CanRename: true, HasInvite: true}
	busy := ready
	busy.Busy = true
	aliased := ready
	aliased.Alias, aliased.Display = "Мой сайт", "Мой сайт"
	connecting := ProjectView{ID: "NBSWY3DPEB3W64TMMQQGC3DUMU", State: ProjectConnecting, Members: []node.MemberInfo{self},
		CanRename: true, HasInvite: true}
	needsFolder := ready
	needsFolder.Dir, needsFolder.State = "", ProjectNeedsFolder
	failed := connecting
	failed.State, failed.Problem = ProjectError, "unknown_project"
	legacy := ProjectView{ID: LegacyProjectID, Legacy: true, Name: "Прежняя сеть", Display: "Прежняя сеть", Dir: `C:\work`,
		State: ProjectReady, Online: 1, Total: 1, Members: []node.MemberInfo{self, bob}, HasInvite: true}

	parts, ids := []string{"alice", "bob"}, []string{"0f1e2d3c4b5a69788796a5b4c3d2e1f0", "a1b2c3d4e5f60718293a4b5c6d7e8f90"}
	msg := node.Message{ID: "5d41402abc4b2a76b9719d911017c592", From: "alice", To: "", Body: "Посмотри вёрстку главной",
		CreatedAt: at, ChatID: "7b8b965ad4bca0e41ab51de7b31363a1", Participants: parts, ParticipantIDs: ids,
		Responders: []string{"bob"}, RootID: "5d41402abc4b2a76b9719d911017c592", ChatMode: node.ChatModeProject, AuthorKind: node.AuthorHuman}
	reply := node.Message{ID: "7d793037a0760186574b0282f2f435e7", From: "bob", Body: "Готово, поправил отступы", ReplyTo: msg.ID,
		JobStatus: node.JobCompleted, CreatedAt: at.Add(2 * time.Minute), ChatID: msg.ChatID, Participants: parts, ParticipantIDs: ids,
		RootID: msg.RootID, AutoDepth: 1, ChatMode: node.ChatModeProject, AuthorKind: node.AuthorAgent}
	out := node.ChatMessage{Seq: 1, Direction: "out", Delivery: []node.Delivery{{Peer: "bob", Status: "sent", State: "answered", At: at.Add(2 * time.Minute)}}, Message: msg}
	in := node.ChatMessage{Seq: 2, Direction: "in", Message: reply}
	info := node.ChatInfo{
		ID: msg.ChatID, Participants: parts, CreatedAt: at, Project: pid, Mode: node.ChatModeProject, ParticipantIDs: ids,
		Title: "Посмотри вёрстку главной", Count: 2, LastSeq: 2, LastMessage: &in, LastAt: reply.CreatedAt,
		Members: []node.ParticipantState{
			{Name: "alice", Self: true, Connected: true, Compatible: true},
			{Name: "bob", Connected: true, Compatible: true, Jobs: []node.JobActivity{{ReplyTo: msg.ID, JobStatus: node.JobRunning,
				Activity: "Read index.html", ActivityInfo: &node.ActivityState{ID: "op1", Type: "read", Text: "index.html", Phase: node.PhaseRunning, StartedAt: at.Add(time.Minute), Seq: 3},
				UpdatedAt: at.Add(time.Minute)}}},
		},
		Active: true,
	}
	chat := ChatInfoView{ChatInfo: info, Project: pid}
	closed := chat
	closed.CloseID, closed.ClosedBy, closed.ClosedAt = "e4da3b7fbbce2345d7772b0674a318d5", "alice", at.Add(time.Hour)
	closed.Closed, closed.Archived, closed.Active = true, true, false
	closed.Members = []node.ParticipantState{{Name: "alice", Self: true, Connected: true, Compatible: true}, {Name: "bob", Compatible: true}}
	sess := node.Session{SessionID: "claude-1f2e", Provider: "claude", Folder: `C:\work\site`,
		Wake: "hook", TTLSec: 600, RegisteredAt: at, LastSeen: at.Add(time.Minute), Primary: true}
	session := SessionView{Session: sess, Project: pid}

	return map[string]any{
		"projects":             []ProjectView{aliased, connecting, failed, needsFolder, ready, legacy},
		"project_ready":        ready,
		"project_busy":         busy,
		"project_connecting":   connecting,
		"project_needs_folder": needsFolder,
		"project_error":        failed,
		"project_legacy":       legacy,
		"invite":               InviteView{Invite: config.FormatInvite(pid, config.ProjectEpoch, "KRUGS4ZANFZSAYJAONSWG4TFOQ")},
		"invite_legacy":        InviteView{Invite: "K7Q2-MXPA-4RTB"},
		"join_created":         JoinResult{Project: connecting, Created: true},
		"join_existing":        JoinResult{Project: ready, Created: false},
		"chats":                []ChatInfoView{chat},
		"chats_archive":        []ChatInfoView{closed},
		"chat":                 chat,
		"chat_closed":          closed,
		"chat_messages":        []node.ChatMessage{out, in},
		"send_result":          msg,
		"sessions":             []SessionView{session},
	}
}

// TestContractFixtures keeps the projects API contract files equal to what
// the Go types serve. UPDATE_FIXTURES=1 rewrites them.
func TestContractFixtures(t *testing.T) {
	update := os.Getenv("UPDATE_FIXTURES") == "1"
	files := map[string][]byte{}
	for name, v := range contractValues() {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		files[name+".json"] = append(data, '\n')
	}
	for code, status := range contractErrors {
		rec := httptest.NewRecorder()
		writeCodedError(rec, status, code)
		if rec.Code != status {
			t.Fatalf("%s: status %d", code, rec.Code)
		}
		var body apiError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != code || body.Error == uiStrings["error.internal"] {
			t.Fatalf("%s: body %s", code, rec.Body)
		}
		data, err := json.MarshalIndent(map[string]any{"status": status, "body": body}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		files["error_"+code+".json"] = append(data, '\n')
	}
	if update {
		if err := os.MkdirAll(contractDir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range files {
		path := filepath.Join(contractDir, name)
		if update {
			if err := os.WriteFile(path, want, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Errorf("%s: %v (run with UPDATE_FIXTURES=1)", name, err)
			continue
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s differs from the Go types (run with UPDATE_FIXTURES=1):\n%s", name, want)
		}
	}
	entries, err := os.ReadDir(contractDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if files[e.Name()] == nil {
			t.Errorf("%s is no longer part of the contract", e.Name())
		}
	}
}
