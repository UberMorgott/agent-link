//go:build windows

package main

import "testing"

// findAgent walks past cmd.exe (the plugin's agentlink.cmd) and shells to the
// agent: the waiter's liveness is the agent's, not its parent's.
func TestFindAgent(t *testing.T) {
	procs := map[uint32]procEntry{
		10: {parent: 9, name: "agentlink"},
		9:  {parent: 8, name: "cmd"},
		8:  {parent: 7, name: "bash"},
		7:  {parent: 6, name: "claude"},
		6:  {parent: 1, name: "explorer"},
		// a node.exe running Claude Code
		20: {parent: 21, name: "agentlink"},
		21: {parent: 22, name: "cmd"},
		22: {parent: 1, name: "node"},
		// a node.exe running something else, and a loop
		30: {parent: 31, name: "agentlink"},
		31: {parent: 32, name: "node"},
		32: {parent: 30, name: "cmd"},
	}
	cmdline := func(pid uint32) string {
		if pid == 22 {
			return `"C:\node.exe" C:\npm\node_modules\@anthropic-ai\claude-code\cli.js`
		}
		return `"C:\node.exe" server.js`
	}
	for _, c := range []struct {
		from   uint32
		client string
		want   uint32
	}{
		{10, "claude", 7},
		{10, "codex", 0},
		{20, "claude", 22},
		{30, "claude", 0},
		{99, "claude", 0},
	} {
		if got := findAgent(procs, c.from, c.client, cmdline); got != c.want {
			t.Errorf("findAgent(%d, %s) = %d, want %d", c.from, c.client, got, c.want)
		}
	}
}
