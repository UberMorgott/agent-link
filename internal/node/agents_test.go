package node

import (
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestToolKind(t *testing.T) {
	for tool, want := range map[string]string{
		"Edit": AgentEditing, "Write": AgentEditing, "MultiEdit": AgentEditing, "apply_patch": AgentEditing, "fileChange": AgentEditing,
		"Bash": AgentCommand, "PowerShell": AgentCommand, "shell": AgentCommand, "commandExecution": AgentCommand,
		"Read": AgentReading, "Grep": AgentReading, "Glob": AgentReading, "WebSearch": AgentReading,
		"mcp__plugin_agent-link_agentlink__discuss": AgentWaiting, "mcp__agentlink__discuss": AgentWaiting,
		"mcp__agentlink__send": AgentThinking, "mcp__other__discuss": AgentThinking, "Task": AgentThinking, "": AgentThinking,
	} {
		if got := ToolKind(tool); got != want {
			t.Errorf("ToolKind(%q) = %q, want %q", tool, got, want)
		}
	}
}

// Each agent is one entry: a seat by its label (a running node turn by its
// stream, a live session by its hooks, none: off), then every live session no
// seat holds, with its subagents; a pause pauses them all.
func TestAgentStatuses(t *testing.T) {
	p := newTestProject(t)
	n := newProjectNode(t, "a", p, listen(t), nil)
	folder := t.TempDir()
	n.SetFolders(folder, nil)
	for _, req := range []SessionRequest{
		{SessionID: "seat-claude", Provider: ProviderClaude, Folder: folder},
		{SessionID: "free-codex", Provider: ProviderCodex, Folder: folder},
		{SessionID: "free-claude", Provider: ProviderClaude, Folder: folder, Agents: []string{"a1", "a2", "a3"}},
		{SessionID: "idle-claude", Provider: ProviderClaude, Folder: folder, Idle: true},
	} {
		if _, err := n.RegisterSession(req); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.SessionDoing("seat-claude", SessionDoingRequest{Doing: AgentEditing}); err != nil {
		t.Fatal(err)
	}
	if err := n.SessionDoing("free-codex", SessionDoingRequest{Doing: AgentWaiting, Subagents: 5}); err != nil {
		t.Fatal(err)
	}
	if err := n.SessionDoing("nobody", SessionDoingRequest{Doing: AgentEditing}); err == nil {
		t.Fatal("doing of an unknown session taken")
	}
	if err := n.SessionDoing("free-codex", SessionDoingRequest{Doing: AgentIdle}); err == nil {
		t.Fatal("a doing that is no busy state taken")
	}
	n.seats.mu.Lock()
	n.seats.seats = []*Seat{
		{ID: "s1", Provider: ProviderCodex, Label: "Codex 2", SessionID: "old-thread"},
		{ID: "s2", Provider: ProviderClaude, Label: "Claude", SessionID: "seat-claude"},
		{ID: "s3", Provider: ProviderClaude, Label: "Claude 2"},
		{ID: "s4", Provider: ProviderCodex, Label: "Codex 3", Stopped: true},
	}
	n.seats.run["s1"] = func() {}
	n.seats.mu.Unlock()
	n.setSeatDoing("s1", AgentThinking, 2)

	got := n.AgentStatuses("")
	type row struct {
		provider, seat, state string
		subs                  int
	}
	var rows []row
	for _, a := range got {
		rows = append(rows, row{a.Provider, a.Seat, a.State, a.Subagents})
		if a.State != AgentOff && a.State != AgentStopped && a.Since.IsZero() {
			t.Errorf("%+v: no since", a)
		}
	}
	want := []row{
		{ProviderCodex, "Codex 2", AgentThinking, 2},
		{ProviderClaude, "Claude", AgentEditing, 0},
		{ProviderClaude, "Claude 2", AgentOff, 0},
		{ProviderCodex, "Codex 3", AgentStopped, 0},
		{ProviderCodex, "", AgentWaiting, 5},
		{ProviderClaude, "", AgentThinking, 3},
		{ProviderClaude, "", AgentIdle, 0},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("agents\n got %+v\nwant %+v", rows, want)
	}

	// The running seat's turn waits for a discuss answer (it lent its slot).
	n.seats.mu.Lock()
	n.seats.lent["s1"] = true
	n.seats.mu.Unlock()
	if a := n.AgentStatuses("")[0]; a.State != AgentWaiting {
		t.Fatalf("lent seat: %+v", a)
	}

	n.SetStopped(true)
	for _, a := range n.AgentStatuses("") {
		if a.State != AgentPaused {
			t.Fatalf("paused node's agent %+v", a)
		}
	}
	n.SetStopped(false)

	// The node's own member entry carries them.
	if ms := n.Members(); len(ms[0].Agents) != len(want) {
		t.Fatalf("own member agents %+v", ms[0].Agents)
	}
}

// A turn's start or end forgets what the session did in the last one.
func TestSessionDoingResetsOnTurn(t *testing.T) {
	lnA := listen(t)
	n := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, nil)
	folder := t.TempDir()
	n.SetFolders(folder, nil)
	reg := func(idle bool) {
		t.Helper()
		if _, err := n.RegisterSession(SessionRequest{SessionID: "s", Provider: ProviderCodex, Folder: folder, Idle: idle}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() AgentStatus {
		t.Helper()
		list := n.AgentStatuses("")
		if len(list) != 1 {
			t.Fatalf("agents %+v", list)
		}
		return list[0]
	}
	reg(false)
	if err := n.SessionDoing("s", SessionDoingRequest{Doing: AgentCommand, Subagents: 1}); err != nil {
		t.Fatal(err)
	}
	reg(false) // a heartbeat keeps it
	if a := state(); a.State != AgentCommand || a.Subagents != 1 {
		t.Fatalf("heartbeat lost doing: %+v", a)
	}
	reg(true)
	if a := state(); a.State != AgentIdle || a.Subagents != 0 {
		t.Fatalf("idle: %+v", a)
	}
	reg(false)
	if a := state(); a.State != AgentThinking {
		t.Fatalf("next turn: %+v", a)
	}
}

// A peer's agents reach Members only from a peer with CapAgentStatus, and a
// malformed list is dropped whole; an older peer keeps its counts alone.
func TestReceiveAgentsCompat(t *testing.T) {
	n := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	list := []AgentStatus{{Provider: ProviderCodex, State: AgentCommand, Subagents: 2, Since: time.Date(2026, 9, 29, 10, 0, 0, 5, time.UTC)}}
	newer := &peerConn{caps: []string{CapPresence, CapAgentCounts, CapAgentStatus}}
	n.receiveAgents(newer, list)
	if len(newer.agents) != 1 || newer.agents[0].Since.Nanosecond() != 0 {
		t.Fatalf("newer peer's agents %+v", newer.agents)
	}
	older := &peerConn{caps: []string{CapPresence, CapAgentCounts}}
	n.receiveAgents(older, list)
	if older.agents != nil {
		t.Fatalf("older peer's agents kept %+v", older.agents)
	}
	for _, bad := range [][]AgentStatus{
		{{Provider: ProviderCodex, State: "reading /etc/passwd"}},
		{{Provider: "", State: AgentIdle}},
		{{Provider: ProviderCodex, State: AgentIdle, Subagents: -1}},
		{{Provider: ProviderCodex, State: AgentIdle, Seat: strings.Repeat("x", 65)}},
	} {
		n.receiveAgents(newer, bad)
		if newer.agents != nil {
			t.Fatalf("malformed %+v kept", bad)
		}
	}
	// The wire field is new and optional: an older node ignores it.
	b, err := json.Marshal(frame{Type: framePresence, Agents: list})
	if err != nil || !strings.Contains(string(b), `"agents":[{"provider":"codex","state":"command","subagents":2`) {
		t.Fatalf("frame %s %v", b, err)
	}
	var old struct {
		Type     string         `json:"type"`
		Presence []AreaPresence `json:"presence"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.Type != framePresence {
		t.Fatalf("older decode %+v %v", old, err)
	}
}

// Two connected nodes: a sees b's agents one by one, with what they do.
func TestMembersAgentsFromPresence(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	workB := t.TempDir()
	a.SetFolders(t.TempDir(), nil)
	b.SetFolders(workB, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	if _, err := b.RegisterSession(SessionRequest{SessionID: "peer", Provider: ProviderCodex, Folder: workB}); err != nil {
		t.Fatal(err)
	}
	if err := b.SessionDoing("peer", SessionDoingRequest{Doing: AgentEditing, Subagents: 3}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a sees b's agent at work", func() bool {
		for _, m := range a.Members() {
			if m.Name == "b" {
				return len(m.Agents) == 1 && m.Agents[0].State == AgentEditing && m.Agents[0].Subagents == 3
			}
		}
		return false
	})
}

// A node turn's stream tells what its agent does: Claude's tool calls and
// running Task subagents (not a subagent's own calls), Codex's items.
func TestStreamDoing(t *testing.T) {
	var got []string
	tell := func(kind string, subs int) { got = append(got, kind+"/"+strconv.Itoa(subs)) }
	in := strings.Join([]string{
		`{"type":"system","session_id":"s-1"}`,
		`{"type":"user","message":{"role":"user","content":"plain prompt"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Task"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"tool_use","id":"x","name":"Bash"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s-1"}`,
	}, "\n")
	id, err := readClaudeStream(strings.NewReader(in), func(string) {}, tell, nil)
	if err != nil || id != "s-1" || !slices.Equal(got, []string{"thinking/1", "edit/1", "thinking/1", "thinking/0"}) {
		t.Fatalf("claude: %q %v %v", id, err, got)
	}
	got = nil
	last := ""
	for _, ev := range []struct{ method, params string }{
		{"item/started", `{"item":{"type":"reasoning"}}`},
		{"item/started", `{"item":{"type":"commandExecution","command":"go test"}}`},
		{"item/completed", `{"item":{"type":"commandExecution"}}`},
		{"item/started", `{"item":{"type":"mcpToolCall","server":"agentlink","tool":"discuss"}}`},
	} {
		codexDoing(tell, ev.method, json.RawMessage(ev.params), &last)
	}
	if !slices.Equal(got, []string{"thinking/0", "command/0", "thinking/0", "waiting/0"}) {
		t.Fatalf("codex: %v", got)
	}
}
