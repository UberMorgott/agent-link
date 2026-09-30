package main

import (
	"os"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	clearAgentEnv()
	// The hook tests act as an interactive session even when `go test` runs
	// under a headless agent (claude -p, codex exec).
	hookHeadless = func(string) bool { return false }
	// Nor do they read the plugin state of the person running them.
	pluginNotice = func(string) string { return "" }
	goleak.VerifyTestMain(m)
}

// clearAgentEnv keeps the tests hermetic: the agent session that runs go test
// (Claude Code, Codex, an AgentLink seat) must not steer them.
// A test's own helper process (AGENTLINK_TEST_*, AGENTLINK_FAKE_*) keeps the
// environment its test gave it.
func clearAgentEnv() {
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AGENTLINK_TEST_") || strings.HasPrefix(kv, "AGENTLINK_FAKE_") {
			return
		}
	}
	for _, k := range []string{"AGENTLINK_API", "AGENTLINK_PROJECT_ID", "AGENTLINK_CHAT_ID", "AGENTLINK_JOB_ID", "AGENTLINK_SEAT",
		"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", envInboxSocket, envInboxToken, envAttended} {
		_ = os.Unsetenv(k)
	}
}
