package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Claude Code's cross-session inbox
// (https://code.claude.com/docs/en/cross-session-messaging.md, "The
// session's inbox socket"): every interactive session listens on a named pipe
// (Windows, \\.\pipe\LOCAL\cc-msg-<hex>) or a Unix socket and exports its
// address and a per-session token to its hooks (CLAUDE_CODE_MESSAGING_SOCKET,
// CLAUDE_CODE_MESSAGING_TOKEN). The hooks register both with the node
// (SessionRequest.InboxSocket, InboxToken); the node keeps them in memory
// only. To wake the idle session it opens one connection and writes two JSON
// lines, then closes it (the session answers nothing):
//
//	{"type":"auth","token":"<token>"}
//	{"type":"user","message":{"role":"user","content":"<prompt>"}}
//
// The auth line is required on Windows and proves the message comes from the
// session's own child (a hook), so Claude Code delivers it unless the
// session's crossSessionInbound setting says otherwise. An idle session starts
// a turn with the prompt; its UserPromptSubmit hook then delivers the unread
// messages as always. Claude Code closes a connection that sends no complete
// line within 30 s, so the connection is opened only when the lines are ready.

// Providers of agent sessions the node wakes or opens.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// inboxAddr is a Claude session's inbox, in memory only; woke is when the
// node last posted a wake there.
type inboxAddr struct {
	socket, token string
	woke          time.Time
}

// inboxWakeGrace: a session still idle this long after a successful inbox
// wake did not take it (its crossSessionInbound setting may drop posts, or
// it sits in a dialog); its background waiter wakes it from then on.
const inboxWakeGrace = 2 * time.Minute

// inboxPostTimeout bounds one post to an inbox.
const inboxPostTimeout = 5 * time.Second

var (
	inboxPipePattern  = regexp.MustCompile(`^\\\\\.\\pipe\\LOCAL\\cc-msg-[0-9a-f]{8,64}$`)
	inboxTokenPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{16,128}$`)
)

// validInboxSocket accepts a Claude Code inbox address only: its named pipe
// on Windows, an absolute socket path named cc-msg-* elsewhere. The node
// writes the session's token there, so nothing else may be named.
func validInboxSocket(s string) bool {
	if runtime.GOOS == "windows" {
		return inboxPipePattern.MatchString(s)
	}
	return len(s) <= 1024 && filepath.IsAbs(s) && strings.HasPrefix(filepath.Base(s), "cc-msg-")
}

func validInboxToken(t string) bool { return inboxTokenPattern.MatchString(t) }

// InboxPoster posts a prompt to a Claude session's inbox.
type InboxPoster interface {
	Post(ctx context.Context, socket, token, text string) error
}

// SetInboxPoster sets how the node posts to Claude inboxes (PipePoster); nil
// (the default) leaves idle Claude sessions to their background waiter.
func (n *Node) SetInboxPoster(p InboxPoster) { n.poster = p }

// PipePoster posts to a Claude inbox over its named pipe (Unix socket).
type PipePoster struct{}

// Post writes the auth line and the prompt to the inbox at socket.
func (PipePoster) Post(ctx context.Context, socket, token, text string) error {
	ctx, cancel := context.WithTimeout(ctx, inboxPostTimeout)
	defer cancel()
	c, err := dialInbox(ctx, socket)
	if err != nil {
		return fmt.Errorf("open inbox: %w", err)
	}
	defer func() { _ = c.Close() }()
	return writeInbox(c, token, text)
}

// writeInbox writes the two lines of one inbox post to w.
func writeInbox(w io.Writer, token, text string) error {
	if token == "" || text == "" {
		return errors.New("inbox post needs a token and a text")
	}
	auth, err := json.Marshal(map[string]string{"type": "auth", "token": token})
	if err != nil {
		return err
	}
	msg, err := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(auth)+len(msg)+2)
	buf = append(append(append(append(buf, auth...), '\n'), msg...), '\n')
	_, err = w.Write(buf)
	return err
}

// InboxWakes reports whether the node wakes session sid through its inbox
// (so its background waiter must not wake it too): it holds the inbox, and
// the session did not stay idle past inboxWakeGrace after the node woke it.
func (n *Node) InboxWakes(sid string) bool {
	if n.poster == nil {
		return false
	}
	r := n.sess
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.inbox[sid]
	if !ok {
		return false
	}
	s := r.sessions[sid]
	return s == nil || !s.Woken || !s.Idle || a.woke.IsZero() || time.Since(a.woke) <= inboxWakeGrace
}
