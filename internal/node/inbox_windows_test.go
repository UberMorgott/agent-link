package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// fakeInbox serves one connection on a named pipe like Claude Code's inbox and
// returns the lines it read.
func fakeInbox(t *testing.T, name string) <-chan []string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, waiting := make(chan []string, 1), make(chan struct{})
	go func() {
		f := os.NewFile(uintptr(h), name)
		defer func() { _ = f.Close() }()
		close(waiting)
		if err := windows.ConnectNamedPipe(h, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			out <- nil
			return
		}
		var lines []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		out <- lines
	}()
	// Like Claude Code, the server waits in ConnectNamedPipe before a client
	// comes (a client that connects and leaves earlier would lose its data).
	<-waiting
	time.Sleep(100 * time.Millisecond)
	return out
}

// TestPipePosterLive posts to a real Claude Code session's inbox, only when
// AGENTLINK_E2E_INBOX_SOCKET and AGENTLINK_E2E_INBOX_TOKEN name one (a scratch
// session started for the test): the session starts a turn with the text.
func TestPipePosterLive(t *testing.T) {
	sock, tok := os.Getenv("AGENTLINK_E2E_INBOX_SOCKET"), os.Getenv("AGENTLINK_E2E_INBOX_TOKEN")
	if sock == "" || tok == "" {
		t.Skip("no live inbox given")
	}
	text := os.Getenv("AGENTLINK_E2E_INBOX_TEXT")
	if text == "" {
		text = "Reply exactly WOKEN"
	}
	if err := (PipePoster{}).Post(context.Background(), sock, tok, text); err != nil {
		t.Fatal(err)
	}
}

// PipePoster writes the auth line and the user message to the pipe and
// closes it; a missing pipe is an error.
func TestPipePoster(t *testing.T) {
	name := `\\.\pipe\LOCAL\cc-msg-` + randomHex(16)
	got := fakeInbox(t, name)
	if !validInboxSocket(name) {
		t.Fatalf("test pipe name %q not valid", name)
	}
	if err := (PipePoster{}).Post(context.Background(), name, testToken, "agent-link: wake"); err != nil {
		t.Fatal(err)
	}
	var lines []string
	select {
	case lines = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the inbox read nothing")
	}
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(lines[0]), &auth) != nil || auth.Type != "auth" || auth.Token != testToken {
		t.Fatalf("auth line %q", lines[0])
	}
	if json.Unmarshal([]byte(lines[1]), &msg) != nil || msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != "agent-link: wake" {
		t.Fatalf("message line %q", lines[1])
	}
	if err := (PipePoster{}).Post(context.Background(), `\\.\pipe\LOCAL\cc-msg-`+randomHex(16), testToken, "x"); err == nil {
		t.Fatal("posted to a missing pipe")
	}
}
