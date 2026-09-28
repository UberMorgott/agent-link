package main

import (
	"slices"
	"strings"
	"testing"
	"time"
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
