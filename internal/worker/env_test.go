package worker

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A job's agent gets the node's API, so its agentlink CLI needs no --config,
// a project context's job its project, and a chat job also its chat and request.
func TestAgentEnv(t *testing.T) {
	w := &Worker{opt: Options{API: "127.0.0.1:7520"}}
	if got := w.agentEnv("", "j1"); !slices.Equal(got, []string{"AGENTLINK_API=127.0.0.1:7520"}) {
		t.Fatalf("plain job env = %q", got)
	}
	want := []string{"AGENTLINK_API=127.0.0.1:7520", "AGENTLINK_CHAT_ID=c1", "AGENTLINK_JOB_ID=j1"}
	if got := w.agentEnv("c1", "j1"); !slices.Equal(got, want) {
		t.Fatalf("chat job env = %q", got)
	}
	if got := (&Worker{}).agentEnv("", "j1"); len(got) != 0 {
		t.Fatalf("no api, no chat: env = %q", got)
	}
	// A project context's agent also gets its project.
	w.opt.ProjectID = "PID"
	want = []string{"AGENTLINK_API=127.0.0.1:7520", "AGENTLINK_PROJECT_ID=PID", "AGENTLINK_CHAT_ID=c1", "AGENTLINK_JOB_ID=j1"}
	if got := w.agentEnv("c1", "j1"); !slices.Equal(got, want) {
		t.Fatalf("project chat job env = %q", got)
	}
}

func TestChatInputShowsCommandsWithoutConfig(t *testing.T) {
	chats := newFakeChats()
	m := chats.post(node.Message{ID: "r1", From: "peer", Body: "q"})
	w := &Worker{opt: Options{Chats: chats, Self: "me", API: "127.0.0.1:7520"}, log: slog.New(slog.DiscardHandler)}
	text, _ := w.chatInput(m, 0, false)
	if !strings.Contains(text, "$AGENTLINK_API are set, no --config needed") || strings.Contains(text, "--config <") {
		t.Fatalf("chat input:\n%s", text)
	}
}
