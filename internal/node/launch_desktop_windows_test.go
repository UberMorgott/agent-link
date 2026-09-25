package node

import (
	"cmp"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDesktopLauncherLive runs a real first turn and opens it in the desktop
// app, only when AGENTLINK_E2E_DESKTOP_DIR names a scratch folder: provider
// AGENTLINK_E2E_DESKTOP_PROVIDER (default claude), resuming
// AGENTLINK_E2E_DESKTOP_RESUME when set, with prompt
// AGENTLINK_E2E_DESKTOP_PROMPT.
func TestDesktopLauncherLive(t *testing.T) {
	dir := os.Getenv("AGENTLINK_E2E_DESKTOP_DIR")
	if dir == "" {
		t.Skip("no desktop folder given")
	}
	spec := LaunchSpec{Provider: os.Getenv("AGENTLINK_E2E_DESKTOP_PROVIDER"), Folder: dir,
		ResumeID: os.Getenv("AGENTLINK_E2E_DESKTOP_RESUME"), Prompt: os.Getenv("AGENTLINK_E2E_DESKTOP_PROMPT")}
	if spec.Provider == "" {
		spec.Provider = ProviderClaude
	}
	l := DesktopLauncher{Version: "e2e"}
	if !l.Direct(spec.Provider) {
		t.Fatalf("no desktop app for %s", spec.Provider)
	}
	err := l.Run(context.Background(), spec, func(id string) { t.Logf("started session %s", id) })
	if err != nil {
		t.Fatal(err)
	}
}

// TestDesktopLaunchLiveNode runs the whole desktop launch of a node against
// the real agent, only when AGENTLINK_E2E_NODE_DIR names a scratch folder:
// provider AGENTLINK_E2E_DESKTOP_PROVIDER (default claude). With
// AGENTLINK_E2E_FAIL=1 the turn is made to fail (an unknown model): the
// message must stay unread, the claim dropped, Terminal opened once instead.
func TestDesktopLaunchLiveNode(t *testing.T) {
	dir := os.Getenv("AGENTLINK_E2E_NODE_DIR")
	if dir == "" {
		t.Skip("no node folder given")
	}
	provider := os.Getenv("AGENTLINK_E2E_DESKTOP_PROVIDER")
	fail := os.Getenv("AGENTLINK_E2E_FAIL") == "1"
	if fail {
		t.Setenv("ANTHROPIC_MODEL", "claude-no-such-model-e2e")
	}
	term := &fakeLauncher{}
	l := DesktopLauncher{Terminal: term, Version: "e2e"}
	if !l.Direct(cmp.Or(provider, ProviderClaude)) {
		t.Fatalf("no desktop app for %q", provider)
	}
	a, b := deliveryPair(t, dir, nil, l)
	a.deliv.mu.Lock()
	a.deliv.provider = cmp.Or(provider, ProviderClaude)
	a.deliv.mu.Unlock()
	m := ask(t, a, b, "e2e agent-link: ответь одним словом OK. Ничего не запускай и не отвечай через agentlink.")
	a.launchDue(context.Background(), time.Now().Add(launchGrace+time.Second))
	a.directWG.Wait()
	got := attemptsOf(b, m)
	eventually(t, "attempts", func() bool { got = attemptsOf(b, m); return len(got) == 2 })
	a.sess.mu.Lock()
	last := a.sess.recent[""]
	a.sess.mu.Unlock()
	t.Logf("attempts %v, terminal launches %d, last session %+v", got, len(term.all()), last)
	unread, _ := a.Unread("", "", 10)
	if fail {
		if !strings.HasPrefix(got[1], AttemptLaunchFailed+":") || len(unread.Messages) != 1 || len(a.launchHeld()) != 0 || len(term.all()) != 1 {
			t.Fatalf("failed launch: attempts %v unread %d held %v terminal %d", got, len(unread.Messages), a.launchHeld(), len(term.all()))
		}
		return
	}
	if got[1] != AttemptLaunchConfirmed || len(unread.Messages) != 0 || len(term.all()) != 0 {
		t.Fatalf("launch: attempts %v unread %d terminal %d", got, len(unread.Messages), len(term.all()))
	}
}
