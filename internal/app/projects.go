package app

import (
	"encoding/json"
	"net/http"

	"github.com/UberMorgott/agent-link/internal/node"
)

// The web API of projects (docs/plans/projects-v1.md §7). These types are the
// contract with the web UI: internal/app/testdata/projects holds their JSON as
// served, checked by TestContractFixtures.

// LegacyProjectID stands for the pre-projects network in {pid} and in
// ChatInfoView.Project.
const LegacyProjectID = "legacy"

// Project states (ProjectView.State).
const (
	ProjectConnecting  = "connecting"   // the shared name has not arrived yet
	ProjectNeedsFolder = "needs_folder" // no folder bound: chats work, the agent never runs
	ProjectReady       = "ready"        // name known and folder bound; says nothing about peers online
	ProjectError       = "error"        // exactly while Problem != ""
)

// ProjectView is one project (or the legacy network) as the web UI lists it.
type ProjectView struct {
	ID      string `json:"id"` // project id or LegacyProjectID
	Legacy  bool   `json:"legacy"`
	Name    string `json:"name"`    // shared name; "" while connecting
	Alias   string `json:"alias"`   // this member's own name for it; "" = none
	Display string `json:"display"` // Alias, else Name
	Dir     string `json:"dir"`     // bound folder; "" = none
	State   string `json:"state"`
	// Problem is "" or a code: unknown_project, auth, name_taken, removed.
	Problem string `json:"problem"`
	// Online and Total count the other members: Members lists this node too
	// (self: true), so Total is len(Members)-1.
	Online    int               `json:"online"` // other members with a session
	Total     int               `json:"total"`  // other members, not removed
	Members   []node.MemberInfo `json:"members"`
	CanRename bool              `json:"can_rename"`
	HasInvite bool              `json:"has_invite"`
	Busy      bool              `json:"busy"` // the worker has unfinished jobs
	// Autonomy: how far this member's agents work by themselves in the project
	// (settings.ProjectBinding.Autonomy, autonomy.go); nil for the legacy network.
	Autonomy *AutonomyView `json:"autonomy,omitempty"`
	// LaunchMode: where an opened session opens, "desktop" or "terminal"
	// (settings.ProjectBinding.LaunchMode).
	LaunchMode string `json:"launch_mode,omitempty"`
}

// InviteView is the answer of the invite reveal endpoint, the only way a
// project secret reaches the page.
type InviteView struct {
	Invite string `json:"invite"`
}

// JoinResult answers a join. Created is false when this project (same
// secret) or this legacy code was here already: the page opens it.
type JoinResult struct {
	Project ProjectView `json:"project"`
	Created bool        `json:"created"`
}

// ChatInfoView is a chat as the projects API lists it, with its context.
type ChatInfoView struct {
	node.ChatInfo
	Project string `json:"project"` // project id or LegacyProjectID
}

// SessionView is an agent session with its context.
type SessionView struct {
	node.Session
	Project string `json:"project"` // project id or LegacyProjectID
}

// apiError is the body of every projects API error: one sentence for the
// user (uiStrings["error."+Code]) and the code for the page's logic.
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// writeCodedError answers with status and {"error": <sentence>, "code": code}.
func writeCodedError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: msg("error."+code, nil), Code: code})
}
