package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

func TestDiscussReusesFolderProjectAndChat(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	call := func(folder, provider, body string) discussResultAPI {
		t.Helper()
		code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
			"folder": folder, "provider": provider, "body": body,
		}), nil)
		if code != http.StatusOK {
			t.Fatalf("discuss: %d %s", code, raw)
		}
		var got discussResultAPI
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := call(dir, node.ProviderCodex, "Review this project")
	second := call(filepath.Join(dir, "."), node.ProviderClaude, "Discuss the review")
	if first.Project == "" || first.Chat == "" || first.ID == second.ID || first.Project != second.Project || first.Chat != second.Chat || first.Seat == second.Seat {
		t.Fatalf("project/chat not reused: %+v %+v", first, second)
	}
	var projects []ProjectView
	if code, raw := h.api(t, http.MethodGet, "projects", nil, &projects); code != http.StatusOK || len(projects) != 1 || projects[0].Dir != dir {
		t.Fatalf("projects: %d %s", code, raw)
	}
	var chats []node.ChatInfo
	if code, raw := h.api(t, http.MethodGet, "projects/"+first.Project+"/chats", nil, &chats); code != http.StatusOK || len(chats) != 1 || chats[0].ID != first.Chat {
		t.Fatalf("chats: %d %s", code, raw)
	}
	var seats []node.SeatView
	if code, raw := h.api(t, http.MethodGet, "projects/"+first.Project+"/seats", nil, &seats); code != http.StatusOK || len(seats) != 2 {
		t.Fatalf("seats: %d %s", code, raw)
	}
}

type discussResultAPI struct {
	Project string `json:"project"`
	Chat    string `json:"chat"`
	ID      string `json:"id"`
	Seat    string `json:"seat"`
	Queued  bool   `json:"queued,omitempty"`
}

func TestDiscussReturnsQueuedWhenPaused(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	call := func() discussResultAPI {
		t.Helper()
		code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
			"folder": dir, "provider": node.ProviderCodex, "body": "question",
		}), nil)
		if code != http.StatusOK {
			t.Fatalf("discuss: %d %s", code, raw)
		}
		var got discussResultAPI
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := call()
	if err := h.app.SetStopAll(true); err != nil {
		t.Fatal(err)
	}
	paused := call()
	if !paused.Queued || paused.Chat != first.Chat || paused.Seat != first.Seat {
		t.Fatalf("paused discuss: %+v first: %+v", paused, first)
	}
	path := "/discuss/reply?" + url.Values{"project": {paused.Project}, "chat": {paused.Chat},
		"id": {paused.ID}, "seat": {paused.Seat}, "timeout": {"2s"}}.Encode()
	if code, body := h.do(t, http.MethodGet, path, "", nil); code != http.StatusAccepted || body != "" {
		t.Fatalf("paused reply wait: %d %s", code, body)
	}
}

func TestDiscussFromSeatSessionToOtherAgent(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	post := func(provider string) discussResultAPI {
		t.Helper()
		code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
			"folder": dir, "provider": provider, "body": "setup",
		}), nil)
		if code != http.StatusOK {
			t.Fatalf("discuss setup: %d %s", code, raw)
		}
		var result discussResultAPI
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	codex := post(node.ProviderCodex)
	claude := post(node.ProviderClaude)
	n := h.app.projects[codex.Project].n
	eventuallyApp(t, "source seat setup", func() bool {
		for _, s := range n.Seats() {
			if s.SessionID == "" || s.LastTurn.IsZero() || s.Status == node.SeatRunning {
				return false
			}
		}
		return true
	})
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
		"folder": dir, "provider": node.ProviderCodex, "source": node.ProviderClaude,
		"session_id": "claude-1", "seat": claude.Seat, "body": "Ask Codex",
	}), nil)
	if code != http.StatusOK {
		t.Fatalf("Claude seat asking Codex: %d %s", code, raw)
	}
	var sent discussResultAPI
	if err := json.Unmarshal([]byte(raw), &sent); err != nil || sent.Chat != codex.Chat || sent.Seat != codex.Seat {
		t.Fatalf("discussion: %+v: %v", sent, err)
	}
	msgs, err := n.ChatMessages(sent.Chat, 0, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var fromClaude *node.Message
	for i := range msgs {
		if msgs[i].ID == sent.ID {
			fromClaude = &msgs[i].Message
		}
	}
	if fromClaude == nil || fromClaude.Agent == nil || fromClaude.Agent.Seat != claude.Seat ||
		len(fromClaude.AskSeats) != 1 || fromClaude.AskSeats[0] != codex.Seat {
		t.Fatalf("agent-to-agent message: %+v", fromClaude)
	}
}

func TestDiscussReplyWaitsForAskedSeat(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	post := func(provider string) discussResultAPI {
		t.Helper()
		code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
			"folder": dir, "provider": provider, "body": "What is the answer?",
		}), nil)
		if code != http.StatusOK {
			t.Fatalf("discuss: %d %s", code, raw)
		}
		var result discussResultAPI
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	ask := post(node.ProviderCodex)
	other := post(node.ProviderClaude)
	n := h.app.projects[ask.Project].n
	eventuallyApp(t, "seat setup", func() bool {
		for _, s := range n.Seats() {
			if s.SessionID == "" || s.LastTurn.IsZero() || s.Status == node.SeatRunning {
				return false
			}
		}
		return true
	})
	wrong, err := n.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "wrong seat",
		Seat: other.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if wrong.Agent == nil || wrong.Agent.Seat != other.Seat {
		t.Fatalf("wrong seat identity lost: %+v", wrong.Agent)
	}
	path := func(timeout string) string {
		return "/discuss/reply?" + url.Values{"project": {ask.Project}, "chat": {ask.Chat},
			"id": {ask.ID}, "seat": {ask.Seat}, "timeout": {timeout}}.Encode()
	}
	if code, body := h.do(t, http.MethodGet, path("30ms"), "", nil); code != http.StatusNoContent || body != "" {
		t.Fatalf("other seat satisfied wait: %d %s", code, body)
	}
	type response struct {
		code int
		body string
		err  error
	}
	done := make(chan response, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+path("2s"), nil)
		if err != nil {
			done <- response{err: err}
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		done <- response{code: resp.StatusCode, body: string(body), err: err}
	}()
	eventuallyApp(t, "reply waiter subscribed", func() bool {
		h.app.events.mu.Lock()
		defer h.app.events.mu.Unlock()
		return len(h.app.events.subscribers) == 1
	})
	answer, err := n.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "42",
		Seat: ask.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		var reply node.Message
		if got.err != nil || got.code != http.StatusOK || json.Unmarshal([]byte(got.body), &reply) != nil ||
			reply.ID != answer.ID || reply.ReplyTo != ask.ID || reply.Agent == nil || reply.Agent.Seat != ask.Seat {
			t.Fatalf("wait: %+v reply %+v", got, reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reply wait did not wake")
	}
}

func TestConcurrentDiscussReusesSeat(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	body := jsonOf(t, map[string]string{"folder": dir, "provider": node.ProviderCodex, "body": "review"})
	type response struct {
		status int
		result discussResultAPI
		err    error
	}
	results := make(chan response, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/discuss", strings.NewReader(body))
			if err != nil {
				results <- response{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- response{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			var result discussResultAPI
			err = json.NewDecoder(resp.Body).Decode(&result)
			results <- response{status: resp.StatusCode, result: result, err: err}
		})
	}
	wg.Wait()
	close(results)
	var first discussResultAPI
	for got := range results {
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("concurrent discuss: %+v", got)
		}
		if first.ID == "" {
			first = got.result
		}
		if got.result.Project != first.Project || got.result.Chat != first.Chat || got.result.Seat != first.Seat {
			t.Fatalf("different project/chat/seat: %+v vs %+v", first, got.result)
		}
	}
	if seats := h.app.projects[first.Project].n.Seats(); len(seats) != 1 {
		t.Fatalf("created %d Codex seats, want 1", len(seats))
	}
}
