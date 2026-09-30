package node

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"
)

// Live agent status: what each agent of this node does right now, one entry
// per agent (a seat, or a live session no seat holds), for the agents popover
// and for peers with CapAgentStatus (presence frames). It carries a state
// kind, a provider, a seat label, a subagent count and a time only: never a
// tool's arguments, a path or a command line.

// CapAgentStatus: presence frames carry AgentStatus entries (Frame.Agents).
// Older peers get the per-provider counts alone (CapAgentCounts).
const CapAgentStatus = "agent-status-v1"

// CapAgentSubagents: the peer knows AgentSubagents; an older peer gets
// AgentWaiting for it (forPeer).
const CapAgentSubagents = "agent-subagents-v1"

// Agent states (AgentStatus.State).
const (
	AgentThinking  = "thinking"    // in a turn, no tool known to run
	AgentEditing   = "edit"        // edits files (Edit, Write, apply_patch…)
	AgentCommand   = "command"     // runs a shell command
	AgentReading   = "read"        // reads or searches code, or the web
	AgentWaiting   = "waiting"     // waits for another agent's answer (discuss)
	AgentSubagents = "subagents"   // its turn ended, its subagents still run
	AgentQueued    = "queued"      // its turn waits for a free slot
	AgentIdle      = "idle"        // live, waits for a question (a message wakes it)
	AgentPaused    = "paused"      // the project's (or every) agent is paused
	AgentStopped   = "stopped"     // a seat the person stopped
	AgentOff       = "off"         // a seat with no process: a question starts it
	AgentHuman     = "needs_human" // a seat whose turns keep failing
)

// maxAgentStatus bounds a peer's AgentStatus list and its fields.
const (
	maxAgentStatus      = maxSessions + maxSeats
	maxAgentSubagents   = maxSessionAgents
	maxAgentStatusField = 64
)

// AgentStatus is one agent of a node as the agents popover shows it.
type AgentStatus struct {
	Provider string `json:"provider"`
	// Seat is the seat's label; empty for a session no seat holds.
	Seat  string `json:"seat,omitempty"`
	State string `json:"state"`
	// Subagents: its live subagents (0 when none or unknown).
	Subagents int `json:"subagents,omitempty"`
	// Since: when it entered State (for AgentOff its last turn), UTC seconds.
	Since time.Time `json:"since,omitzero"`
}

// agentStates are the states a peer may send.
var agentStates = []string{AgentThinking, AgentEditing, AgentCommand, AgentReading, AgentWaiting, AgentSubagents, AgentQueued,
	AgentIdle, AgentPaused, AgentStopped, AgentOff, AgentHuman}

// busyStates are the states of an agent at work (the popover's count).
var busyStates = []string{AgentThinking, AgentEditing, AgentCommand, AgentReading, AgentWaiting}

// Busy reports whether a is at work.
func (a AgentStatus) Busy() bool { return slices.Contains(busyStates, a.State) }

// ToolKind is the agent state of a tool call, by the tool's name alone: a
// Claude Code tool, a Codex hook tool or a Codex app-server item type. The
// agent-link discuss tool waits for an answer; an unknown tool is thinking.
func ToolKind(tool string) string {
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch", "fileChange":
		return AgentEditing
	case "Bash", "PowerShell", "shell", "local_shell", "exec_command", "unified_exec", "commandExecution":
		return AgentCommand
	case "Read", "NotebookRead", "Grep", "Glob", "LS", "ToolSearch", "WebFetch", "WebSearch", "web_search", "webSearch":
		return AgentReading
	}
	if strings.HasPrefix(tool, "mcp__") && strings.HasSuffix(tool, "__discuss") && strings.Contains(tool, "agentlink") {
		return AgentWaiting
	}
	return AgentThinking
}

// validDoing reports a state a session's hooks may report (SessionDoing):
// a busy one, or AgentSubagents for an idle session's subagent count alone.
func validDoing(s string) bool {
	return s == AgentSubagents || slices.Contains(busyStates, s)
}

// agentDoing is what a running agent does, since at, with subs subagents.
type agentDoing struct {
	kind string
	at   time.Time
	subs int
}

// SessionDoingRequest is the body of POST /sessions/{id}/doing: what the
// session's main agent does now (a busy state: its hooks' tool kind) and how
// many subagents it runs (a Codex session; Claude's come with its
// registration, SessionRequest.Agents). Doing AgentSubagents reports the
// subagents alone (a Codex session whose turn ended while they run).
type SessionDoingRequest struct {
	Doing     string `json:"doing"`
	Subagents int    `json:"subagents,omitempty"`
}

// SessionDoing keeps what live session sid does now (memory only: no disk
// write, no heartbeat). It tells peers and the page only when it changed.
func (n *Node) SessionDoing(sid string, req SessionDoingRequest) error {
	if !validDoing(req.Doing) || req.Subagents < 0 || req.Subagents > maxAgentSubagents {
		return ErrBadRequest
	}
	r := n.sess
	now := time.Now().UTC()
	r.mu.Lock()
	s := r.sessions[sid]
	if s == nil || !s.live(now) {
		r.mu.Unlock()
		return ErrUnknownSession
	}
	changed := s.Subs != req.Subagents
	if req.Doing != AgentSubagents {
		changed = changed || s.Doing != req.Doing
		if s.Doing != req.Doing {
			s.DoingAt = now
		}
		s.Doing = req.Doing
	}
	s.Subs = req.Subagents
	r.mu.Unlock()
	if changed {
		n.presenceChanged()
		n.changed("sessions")
	}
	return nil
}

// setSeatDoing keeps what seat's running node turn does (the agent's own
// stream: tool names only); kind "" ends it.
func (n *Node) setSeatDoing(seat, kind string, subs int) {
	st := n.seats
	st.mu.Lock()
	old, had := st.doing[seat]
	switch {
	case kind == "":
		delete(st.doing, seat)
	case !had || old.kind != kind || old.subs != subs:
		at := old.at
		if !had || old.kind != kind {
			at = time.Now().UTC()
		}
		st.doing[seat] = agentDoing{kind: kind, at: at, subs: subs}
	default:
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	if kind != "" || had {
		n.presenceChanged()
		n.changed("seats")
	}
}

// setSeatRan keeps the model and reasoning effort seat's turn runs with, as
// its agent reported them at the turn's start (effort empty: not reported).
func (n *Node) setSeatRan(seat, model, effort string) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ran[seat] = AgentRef{Model: model, Effort: effort}
}

// sessionAgent is the status of live session s (its hooks' reports).
func sessionAgent(s Session, now time.Time) AgentStatus {
	a := AgentStatus{Provider: s.Provider, Subagents: max(len(s.liveAgents(now)), s.Subs)}
	if s.Idle {
		// Its turn ended; subagents it started still run: it waits for them.
		a.State, a.Since = AgentIdle, s.activeAt()
		if a.Subagents > 0 {
			a.State = AgentSubagents
		}
		return a
	}
	a.State = cmp.Or(s.Doing, AgentThinking)
	a.Since = s.DoingAt
	if a.Since.IsZero() {
		a.Since = s.activeAt()
	}
	return a
}

// AgentStatuses lists this node's agents in area: its seats (in a project's
// working folder), then the live sessions no seat holds, oldest first.
func (n *Node) AgentStatuses(area string) []AgentStatus {
	now := time.Now()
	paused := n.Stopped()
	want := n.localArea(area)
	var seats []SeatView
	if n.cfg.Project != "" && area == "" {
		seats = n.Seats()
	}
	sessions := map[string]Session{}
	var loose []Session
	held := map[string]bool{}
	for _, seat := range seats {
		held[seat.SessionID] = seat.SessionID != ""
	}
	for _, s := range n.Sessions() {
		sessions[s.SessionID] = s
		if s.Area == want && !held[s.SessionID] {
			loose = append(loose, s)
		}
	}
	st := n.seats
	st.mu.Lock()
	doing, lent := maps.Clone(st.doing), maps.Clone(st.lent)
	st.mu.Unlock()
	out := make([]AgentStatus, 0, len(seats)+len(loose))
	for _, seat := range seats {
		a := AgentStatus{Provider: seat.Provider, Seat: seat.Label}
		s, live := sessions[seat.SessionID]
		switch {
		case paused || seat.Status == SeatPaused:
			a.State = AgentPaused
		case seat.Status == SeatStopped:
			a.State = AgentStopped
		case seat.Status == SeatRunning && seat.TurnQueued:
			a.State = AgentQueued
		case seat.Status == SeatRunning:
			d := doing[seat.ID]
			a.State, a.Since, a.Subagents = cmp.Or(d.kind, AgentThinking), d.at, d.subs
			if lent[seat.ID] {
				a.State = AgentWaiting
			}
		case live:
			a = sessionAgent(s, now)
			a.Seat = seat.Label
		case seat.Status == SeatNeedsHuman:
			a.State = AgentHuman
		case seat.Status == SeatBusy:
			a.State = AgentIdle // open in the agent's app: its hooks take the messages
		default:
			a.State, a.Since = AgentOff, seat.LastTurn
		}
		out = append(out, a.clean())
	}
	for _, s := range loose {
		a := sessionAgent(s, now)
		if paused {
			a.State = AgentPaused
		}
		out = append(out, a.clean())
	}
	return out
}

// forPeer is list as a peer with caps reads it: one without
// CapAgentSubagents gets AgentWaiting for AgentSubagents (it would drop the
// whole list over a state it does not know).
func forPeer(list []AgentStatus, subagents bool) []AgentStatus {
	if subagents {
		return list
	}
	out := slices.Clone(list)
	for i := range out {
		if out[i].State == AgentSubagents {
			out[i].State = AgentWaiting
		}
	}
	return out
}

// clean rounds Since to UTC seconds: equal states compare equal (presence
// frames are sent again only on a change).
func (a AgentStatus) clean() AgentStatus {
	if !a.Since.IsZero() {
		a.Since = a.Since.UTC().Truncate(time.Second)
	}
	return a
}

// validAgentStatus keeps a peer's list when every entry is well formed.
func validAgentStatus(list []AgentStatus) bool {
	if len(list) > maxAgentStatus {
		return false
	}
	for _, a := range list {
		if a.Provider == "" || len(a.Provider) > 32 || len(a.Seat) > maxAgentStatusField || !slices.Contains(agentStates, a.State) ||
			a.Subagents < 0 || a.Subagents > maxAgentSubagents || strings.ContainsAny(a.Provider+a.Seat, "\r\n") {
			return false
		}
	}
	return true
}
