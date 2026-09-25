//go:build seatse2e

package node

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSeatsE2E runs real Claude Code and Codex seats of one project node: a
// person asks the Claude seat to ask the Codex seat, Codex answers Claude,
// and Claude reports. It needs claude and codex on PATH and spends their
// quota, so it runs only by hand:
//
//	$env:SEATS_E2E_DIR = "<scratch folder>"
//	go test -tags seatse2e -run TestSeatsE2E -timeout 30m -v ./internal/node
//
// It writes the chat to <dir>\transcript.txt and archives the Codex threads.
func TestSeatsE2E(t *testing.T) {
	root := os.Getenv("SEATS_E2E_DIR")
	if root == "" {
		t.Skip("SEATS_E2E_DIR not set")
	}
	folder, data, bin := filepath.Join(root, "project"), filepath.Join(root, "data"), filepath.Join(root, "bin")
	for _, d := range []string{folder, data, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(bin, "agentlink.exe"), "../../cmd/agentlink") //nolint:gosec // G204: go build into the test's own folder
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentlink: %v\n%s", err, out)
	}
	a := newProjectNodeAt(t, "morgott", newTestProject(t), data, listen(t), nil)
	a.SetFolders(folder, nil)
	a.SetLauncher(DesktopLauncher{Version: "e2e"}, ProviderClaude)
	a.start(t)
	a.SetSeatEnv([]string{"AGENTLINK_API=" + a.apiLn.Addr().String(), "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")})
	claude, err := a.AddSeat(SeatRequest{Provider: ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	codex, err := a.AddSeat(SeatRequest{Provider: ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Minute, "both seats started", func() bool {
		ok := true
		for _, s := range a.Seats() {
			if s.Error != "" {
				t.Fatalf("seat %s: %s", s.Label, s.Error)
			}
			ok = ok && s.SessionID != "" && s.Status != SeatRunning
		}
		return ok
	})
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	ask, err := a.SendRequest(SendRequest{ChatID: chat.ID, AuthorKind: AuthorHuman, AskSeats: []string{claude.ID},
		Body: "Спроси агента Codex (agentlink send --chat " + chat.ID + " --ask-seat Codex --body \"...\"), сколько будет 17*23. " +
			"Когда его ответ придёт тебе, коротко напиши в этот чат итог со словом ИТОГ."})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("asked Claude: %s", ask.ID)
	var codexReply, final *ChatMessage
	waitFor(t, 20*time.Minute, "Codex answers Claude and Claude reports", func() bool {
		for _, s := range a.Seats() {
			if s.Error != "" {
				t.Fatalf("seat %s: %s", s.Label, s.Error)
			}
		}
		msgs, _ := a.ChatMessages(chat.ID, 0, 0, 200)
		codexReply, final = nil, nil
		for i := range msgs {
			m := &msgs[i]
			if m.Agent == nil || m.Kind != "" {
				continue
			}
			if m.Agent.Seat == codex.ID && m.ReplyTo != "" {
				codexReply = m
			}
			if m.Agent.Seat == claude.ID && codexReply != nil && strings.Contains(m.Body, "ИТОГ") {
				final = m
			}
		}
		return codexReply != nil && final != nil
	})
	msgs, _ := a.ChatMessages(chat.ID, 0, 0, 200)
	var b strings.Builder
	for _, m := range msgs {
		if m.Kind != "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s (reply_to %s, ask_seats %v, depth %d):\n%s\n\n", m.ID[:8], AuthorName(m.Message), short(m.ReplyTo), m.AskSeats, m.AutoDepth, m.Body)
	}
	for _, s := range a.Seats() {
		fmt.Fprintf(&b, "seat %s %s session %s status %s pending %d\n", s.Label, s.Provider, s.SessionID, s.Status, len(s.Pending))
	}
	if err := os.WriteFile(filepath.Join(root, "transcript.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Log(b.String())
	archiveCodex(t, folder, seatByLabel(t, a, "Codex").SessionID)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Second)
	}
}

// archiveCodex archives Codex thread id through its own app-server.
func archiveCodex(t *testing.T, folder, id string) {
	bin, err := exec.LookPath("codex")
	if err != nil || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server") //nolint:gosec // G204: the agent's CLI found on PATH
	cmd.Dir = folder
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Logf("archive codex thread %s: %v", id, err)
		return
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	for _, l := range []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"agentlink-e2e","version":"e2e"}}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"thread/archive","params":{"threadId":"` + id + `"}}`,
	} {
		_, _ = in.Write([]byte(l + "\n"))
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), `{"id":2`) {
			t.Logf("archive codex thread %s: %s", id, sc.Text())
			return
		}
	}
}
