package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A subagent's ask is stamped by its PreToolUse and taken by the tool by its
// input; the reply reaches that subagent's PostToolUse while it lives, never
// the main agent; after its SubagentStop the main agent gets it, marked.
func TestHookRoutesRepliesToTheAskingSubagent(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	const sub = `,"agent_id":"a1","agent_type":"Explore"`
	c.run(hookClaude, evSubagentStart, sub)
	if got := c.f.sessions[c.sid].Agents; !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("node not told of the live subagent: %v", got)
	}

	// The MCP send tool of the subagent: the tool's arguments (other key order,
	// spacing) find the hook's stamp.
	c.run(hookClaude, evPreTool, sub+`,"tool_name":"mcp__plugin_agent-link_agentlink__send","tool_input":{"chat":"c1","body":"q?"}`)
	c.run(hookClaude, evPreTool, `,"tool_name":"mcp__plugin_agent-link_agentlink__send","tool_input":{"chat":"c1","body":"main q"}`)
	key, ok := mcpAskKey("send", []byte(`{ "body": "q?", "chat": "c1" }`))
	if !ok {
		t.Fatal("no key")
	}
	now := c.now
	if agent, typ := takeAskStamp(c.env.dir, c.sid, key, "q?", now); agent != "a1" || typ != "Explore" {
		t.Fatalf("stamp: %q %q", agent, typ)
	}
	if agent, _ := takeAskStamp(c.env.dir, c.sid, key, "q?", now); agent != "" {
		t.Fatalf("stamp taken twice: %q", agent)
	}
	mainKey, _ := mcpAskKey("send", []byte(`{"chat":"c1","body":"main q"}`))
	if agent, _ := takeAskStamp(c.env.dir, c.sid, mainKey, "main q", now); agent != "" {
		t.Fatalf("the main agent's ask got a subagent: %q", agent)
	}

	reply := chatMsg("c1", "KPECTIK", "agent", "answer for the subagent", false)
	reply.ID, reply.ForAgent, reply.ForAgentType = "r1", "a1", "Explore"
	c.f.add(reply)
	if out := c.run(hookClaude, evPostTool, `,"tool_name":"Read"`); strings.Contains(out, "answer for the subagent") {
		t.Fatalf("the main agent got the live subagent's reply: %s", out)
	}
	if out := c.run(hookClaude, evPostTool, `,"agent_id":"a2","agent_type":"Plan","tool_name":"Read"`); strings.Contains(out, "answer for the subagent") {
		t.Fatalf("another subagent got the reply: %s", out)
	}
	out := parseOut(t, c.run(hookClaude, evPostTool, sub+`,"tool_name":"Read"`))
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "answer for the subagent") || out.HookSpecificOutput.HookEventName != evPostTool {
		t.Fatalf("the subagent did not get its reply: %+v", out)
	}
	if !slices.Contains(c.f.ackedIDs(), "r1") {
		t.Fatalf("not acknowledged: %v", c.f.ackedIDs())
	}

	// After the subagent ended its reply is the main agent's, marked.
	late := chatMsg("c1", "KPECTIK", "agent", "late answer", false)
	late.ID, late.ForAgent, late.ForAgentType = "r2", "a1", "Explore"
	c.f.add(late)
	c.run(hookClaude, evSubagentStop, sub)
	if got := c.f.sessions[c.sid].Agents; !slices.Equal(got, []string{"a2"}) {
		t.Fatalf("node's live subagents after the stop: %v", got)
	}
	out = parseOut(t, c.run(hookClaude, evPostTool, `,"tool_name":"Read"`))
	if ctx := out.HookSpecificOutput.AdditionalContext; !strings.Contains(ctx, "late answer") || !strings.Contains(ctx, "завершившегося субагента Explore/a1") {
		t.Fatalf("the main agent did not get the ended subagent's reply: %q", ctx)
	}
}

// A shell ask (agentlink send in Bash) is matched by its command line; a
// subagent silent for liveAgentTTL no longer counts as live.
func TestHookCLIAskStampAndAgentExpiry(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	c.run(hookClaude, evPreTool, `,"agent_id":"a2","agent_type":"Plan","tool_name":"Bash","tool_input":{"command":"agentlink send --chat c1 --body \"hello there friend\""}`)
	c.run(hookClaude, evPreTool, `,"tool_name":"PowerShell","tool_input":{"command":"& agentlink.exe send --body 'other text'"}`)
	c.run(hookClaude, evPreTool, `,"agent_id":"a2","tool_name":"Bash","tool_input":{"command":"go test ./..."}`)
	if n := len(c.state(hookClaude).Stamps); n != 2 {
		t.Fatalf("stamps: %d", n)
	}
	if agent, _ := takeAskStamp(c.env.dir, c.sid, cliAskKey, "other text", c.now); agent != "" {
		t.Fatalf("main CLI ask got %q", agent)
	}
	if agent, typ := takeAskStamp(c.env.dir, c.sid, cliAskKey, "hello there friend", c.now); agent != "a2" || typ != "Plan" {
		t.Fatalf("subagent CLI ask: %q %q", agent, typ)
	}
	if !slices.Equal(liveAgentIDs(c.state(hookClaude)), []string{"a2"}) {
		t.Fatalf("live: %v", c.state(hookClaude).Live)
	}
	c.now = c.now.Add(liveAgentTTL + time.Minute)
	c.run(hookClaude, evPostTool, `,"tool_name":"Read"`)
	if live := c.state(hookClaude).Live; len(live) != 0 {
		t.Fatalf("a silent subagent still live: %v", live)
	}
}

// Never guess: identical asks of two subagents (they may run in either order)
// take no stamp, and the ask stays the parent's; so does a CLI ask whose
// body no stamp holds, or one sent from stdin. One from a file is matched by
// its path.
func TestHookAskStampAmbiguityStaysWithParent(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	const tool = `,"tool_name":"mcp__plugin_agent-link_agentlink__send","tool_input":{"chat":"c1","body":"same"}`
	c.run(hookClaude, evPreTool, `,"agent_id":"a1","agent_type":"Explore"`+tool)
	c.run(hookClaude, evPreTool, `,"agent_id":"a2","agent_type":"Plan"`+tool)
	key, _ := mcpAskKey("send", []byte(`{"chat":"c1","body":"same"}`))
	// a2's call runs first: no stamp may name a1 (or a2).
	if agent, _ := takeAskStamp(c.env.dir, c.sid, key, "same", c.now); agent != "" {
		t.Fatalf("ambiguous ask routed to %q", agent)
	}
	// a1's call ended (PostToolUse drops its stamp): a2's alone is certain.
	c.run(hookClaude, evPostTool, `,"agent_id":"a1","agent_type":"Explore"`+tool)
	if agent, _ := takeAskStamp(c.env.dir, c.sid, key, "same", c.now); agent != "a2" {
		t.Fatalf("after a1 ended: %q", agent)
	}

	c.run(hookClaude, evPreTool, `,"agent_id":"a3","tool_name":"Bash","tool_input":{"command":"agentlink send --chat c1 --body-file q.txt"}`)
	for _, body := range []string{"", "text the command does not hold", askNeedle("", "-")} {
		if agent, _ := takeAskStamp(c.env.dir, c.sid, cliAskKey, body, c.now); agent != "" {
			t.Fatalf("unmatched CLI ask (%q) routed to %q", body, agent)
		}
	}
	// A text file's ask is matched by its path on the command line.
	if agent, _ := takeAskStamp(c.env.dir, c.sid, cliAskKey, askNeedle("", "q.txt"), c.now); agent != "a3" {
		t.Fatalf("--body-file ask: %q", agent)
	}
}

// A stamp does not outlive its call: a failed call (PostToolUseFailure) and
// its subagent's end (SubagentStop) drop it.
func TestHookAskStampDroppedOnFailureAndStop(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	const sub = `,"agent_id":"a1","agent_type":"Explore"`
	const tool = `,"tool_name":"mcp__plugin_agent-link_agentlink__discuss","tool_input":{"with":"codex","body":"q"}`
	c.run(hookClaude, evPreTool, sub+tool)
	c.run(hookClaude, evPostToolFail, sub+tool)
	if n := len(c.state(hookClaude).Stamps); n != 0 {
		t.Fatalf("failed call left %d stamps", n)
	}
	c.run(hookClaude, evPreTool, sub+tool)
	c.run(hookClaude, evPreTool, sub+`,"tool_name":"Bash","tool_input":{"command":"agentlink send --body x"}`)
	c.run(hookClaude, evSubagentStop, sub)
	if st := c.state(hookClaude); len(st.Stamps) != 0 || len(st.Live) != 0 {
		t.Fatalf("after SubagentStop: %+v %+v", st.Stamps, st.Live)
	}
}

// Without a PreToolUse stamp (hooks not installed, older hooks) and in Codex
// (its tool hooks name no agent) an ask is the session's, and a reply for no
// subagent reaches the main agent even while subagents live.
func TestHookAskWithoutStampOrInCodexStaysWithParent(t *testing.T) {
	c := newHookCase(t)
	key, _ := mcpAskKey("send", []byte(`{"chat":"c1","body":"q"}`))
	if agent, _ := takeAskStamp(c.env.dir, c.sid, key, "q", c.now); agent != "" {
		t.Fatalf("no hook state: %q", agent)
	}
	c.run(hookCodex, evSessionStart)
	c.run(hookCodex, evSubagentStart, `,"agent_id":"child-1","agent_type":"reviewer"`)
	c.run(hookCodex, evPreTool, `,"tool_name":"mcp__agentlink__send","tool_input":{"chat":"c1","body":"q"}`)
	if st := c.state(hookCodex); len(st.Stamps) != 0 || len(st.Live) != 0 {
		t.Fatalf("Codex stamped: %+v %+v", st.Stamps, st.Live)
	}
	if agent, _ := takeAskStamp(c.env.dir, c.sid, key, "q", c.now); agent != "" {
		t.Fatalf("Codex ask routed to %q", agent)
	}
	t.Setenv(envClaudeSession, "")
	t.Setenv(envCodexThread, c.sid)
	if agent, _ := askOrigin(key, "q"); agent != "" {
		t.Fatalf("askOrigin in Codex: %q", agent)
	}

	c.run(hookClaude, evSessionStart)
	c.run(hookClaude, evSubagentStart, `,"agent_id":"a1","agent_type":"Explore"`)
	plain := chatMsg("c1", "KPECTIK", "agent", "reply for the session", false)
	plain.ID = "p1"
	c.f.add(plain)
	out := parseOut(t, c.run(hookClaude, evPostTool, `,"tool_name":"Read"`))
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "reply for the session") {
		t.Fatalf("the main agent did not get a reply for no subagent: %+v", out)
	}
}

// An idle parent has no hook events: its waiter lets a silent subagent lapse,
// so the reply for it is the parent's again.
func TestHookWaiterLapsesSilentSubagent(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	st := hookState{Live: map[string]liveAgent{"a1": {Type: "Explore", At: c.now.Add(-liveAgentTTL - time.Minute)}}}
	m := chatMsg("c1", "KPECTIK", "agent", "late", true)
	m.ForAgent = "a1"
	c.f.add(m)
	if !pendingUnread(c.env, c.folder, c.sid, &st) || len(st.Live) != 0 {
		t.Fatalf("a silent subagent held its reply: %+v", st.Live)
	}
}

// A reply the parent was woken with before its subagent showed up is not
// acknowledged by the parent's hooks while that subagent lives, nor by
// another subagent's (an older node sends it unfiltered in Woken).
func TestHookForMeFiltersWoken(t *testing.T) {
	w := node.UnreadMessage{WakeToken: "tok"}
	w.ID, w.ForAgent = "r1", "a1"
	page := node.UnreadPage{Woken: []node.UnreadMessage{w}}
	st := &hookState{Live: map[string]liveAgent{"a1": {}}}
	if got := (&hookSession{st: st}).forMe(page).Woken; len(got) != 0 {
		t.Fatalf("parent kept a live subagent's woken reply: %+v", got)
	}
	if got := (&hookSession{st: st, agent: "a2"}).forMe(page).Woken; len(got) != 0 {
		t.Fatalf("another subagent kept it: %+v", got)
	}
	if got := (&hookSession{st: st, agent: "a1"}).forMe(page).Woken; len(got) != 1 {
		t.Fatalf("its subagent lost it: %+v", got)
	}
	if got := (&hookSession{st: &hookState{}}).forMe(page).Woken; len(got) != 1 {
		t.Fatalf("parent lost an ended subagent's woken reply: %+v", got)
	}
	if len(page.Woken) != 1 {
		t.Fatal("forMe changed its input")
	}
}
