package main

import (
	"slices"
	"testing"
	"time"
)

// The hooks tell the node what the main agent does, by tool kind and only on
// a change: a subagent's tool leaves it alone, a turn's end forgets it, a
// shell agentlink discuss waits for an answer; Codex adds its live children.
func TestHookTellsDoing(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	c.run(hookClaude, evPrompt)
	c.run(hookClaude, evPreTool, `,"tool_name":"Read","tool_input":{"file_path":"a.go"}`)
	c.run(hookClaude, evPostTool, `,"tool_name":"Read"`)
	c.run(hookClaude, evPreTool, `,"tool_name":"Edit","tool_input":{"file_path":"a.go"}`)
	c.run(hookClaude, evPreTool, `,"tool_name":"Edit","tool_input":{"file_path":"b.go"}`) // unchanged: not told
	c.run(hookClaude, evPreTool, `,"agent_id":"x1","agent_type":"Explore","tool_name":"Bash","tool_input":{"command":"go test"}`)
	c.run(hookClaude, evPreTool, `,"tool_name":"Bash","tool_input":{"command":"& agentlink.exe discuss --provider codex --body 'q'"}`)
	c.run(hookClaude, evStop)
	c.run(hookClaude, evPrompt)
	c.run(hookClaude, evPreTool, `,"tool_name":"PowerShell","tool_input":{"command":"go test ./..."}`)
	if want := []string{"read/0", "thinking/0", "edit/0", "waiting/0", "command/0"}; !slices.Equal(c.f.doing, want) {
		t.Fatalf("claude doing %v, want %v", c.f.doing, want)
	}

	c = newHookCase(t)
	c.run(hookCodex, evSessionStart)
	c.run(hookCodex, evPreTool, `,"tool_name":"apply_patch","tool_input":{"command":"*** Update File: a.go"}`)
	c.run(hookCodex, evSubagentStart, `,"agent_id":"k1"`)
	c.run(hookCodex, evSubagentStart, `,"agent_id":"k2"`)
	c.run(hookCodex, evPreTool, `,"tool_name":"shell","tool_input":{"command":["go","test"]}`) // a child's, maybe: thinking
	c.run(hookCodex, evSubagentStop, `,"agent_id":"k1"`)
	if want := []string{"edit/0", "thinking/1", "thinking/2", "thinking/1"}; !slices.Equal(c.f.doing, want) {
		t.Fatalf("codex doing %v, want %v", c.f.doing, want)
	}
}

// A doing report is one local call on a change: it adds little to the tool
// call it precedes, and a node that does not answer holds it up at most
// hookDoingTimeout.
func TestHookDoingLatency(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	h := &hookSession{env: c.env, st: &hookState{}, sid: c.sid, folder: c.folder, client: hookClaude}
	const n = 50
	start := time.Now()
	for i := range n {
		h.tellDoing([]string{"read", "edit"}[i%2])
	}
	per := time.Since(start) / n
	t.Logf("doing report: %v per call (%d calls)", per, n)
	if per > 50*time.Millisecond {
		t.Fatalf("doing report takes %v", per)
	}
	if len(c.f.doing) != n {
		t.Fatalf("reports %d", len(c.f.doing))
	}
}
