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
	"github.com/UberMorgott/agent-link/internal/settings"
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

// A discuss that adds a seat queues its message before the seat's first turn:
// the introduction turn carries the message and does not tell the agent to
// end the turn without answering.
func TestDiscussNewSeatFirstTurnCarriesMessage(t *testing.T) {
	runner := &seatRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
		"folder": t.TempDir(), "provider": node.ProviderCodex, "body": "Review this project",
	}), nil)
	if code != http.StatusOK {
		t.Fatalf("discuss: %d %s", code, raw)
	}
	var first node.LaunchSpec
	waitFor(t, "the seat's first turn", func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		if len(runner.specs) == 0 {
			return false
		}
		first = runner.specs[0]
		return true
	})
	if !strings.Contains(first.Prompt, "Review this project") || !strings.Contains(first.Prompt, "agent-link: вы — агент") {
		t.Fatalf("first turn lacks the introduction or the message: %q", first.Prompt)
	}
	if strings.Contains(first.Prompt, "придут в следующем ходе") {
		t.Fatalf("first turn with a message got the no-message setup text: %q", first.Prompt)
	}
}

func TestDiscussUsesPrivateChatBesideNetworkProject(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	var network ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Shared", "dir": dir}, &network); code != http.StatusOK || network.Scope != "network" {
		t.Fatalf("network project: %d %s", code, raw)
	}
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
		"folder": dir, "provider": node.ProviderCodex, "body": "Private discussion",
	}), nil)
	if code != http.StatusOK {
		t.Fatalf("discuss: %d %s", code, raw)
	}
	var sent discussResultAPI
	if err := json.Unmarshal([]byte(raw), &sent); err != nil || sent.Project == network.ID || sent.Chat == "" {
		t.Fatalf("private discussion: %+v, %v", sent, err)
	}
	var local ProjectView
	if code, raw := h.api(t, http.MethodGet, "projects/"+sent.Project, nil, &local); code != http.StatusOK || local.Scope != "local" || local.HasInvite || local.Dir != dir {
		t.Fatalf("local project: %d %s", code, raw)
	}
	if got, err := route(h.app.routeContexts(), selector{folder: dir}); err != nil || got.id != network.ID {
		t.Fatalf("generic folder should use network project: %+v, %v", got, err)
	}
	if got := h.app.projects[sent.Project].n.Members(); len(got) != 1 || !got[0].Self {
		t.Fatalf("local project advertised remote members: %+v", got)
	}
	if err := h.app.projects[sent.Project].n.AddPeer("127.0.0.1:7420"); err == nil {
		t.Fatal("local project accepted a peer address")
	}
	h.wantError(t, http.MethodPost, "projects/"+sent.Project+"/invite", nil, http.StatusNotFound, "not_found")
	h.wantError(t, http.MethodPost, "projects/"+sent.Project+"/members/add", map[string]any{"addr": "127.0.0.1:7420"}, http.StatusBadRequest, "bad_request")
	var networkChat node.ChatInfo
	if info, err := h.app.projects[network.ID].n.NewProjectChat(nil); err != nil {
		t.Fatal(err)
	} else {
		networkChat = info
	}
	h.wantError(t, http.MethodPost, "projects/"+network.ID+"/send", map[string]any{
		"chat_id": networkChat.ID, "body": "Wrong scope", "ask_seats": []string{sent.Seat},
	}, http.StatusBadRequest, "bad_request")
	if code, _ := h.do(t, http.MethodPost, "/send", jsonOf(t, map[string]any{
		"project": network.ID, "chat_id": networkChat.ID, "body": "Wrong scope", "ask_seats": []string{sent.Seat},
	}), nil); code != http.StatusBadRequest {
		t.Fatalf("generic send addressed seat in network project: %d", code)
	}
	if networkChat.ID == sent.Chat || h.app.projects[network.ID].n.OwnsChat(sent.Chat) || h.app.projects[sent.Project].n.OwnsChat(networkChat.ID) {
		t.Fatal("private and network chats share message history")
	}
	if s, _, err := settings.Load(h.app.path); err != nil || len(s.Bindings) != 2 ||
		s.Bindings[h.app.bindingIndex(network.ID)].ScopeOf() != settings.ProjectScopeNetwork ||
		s.Bindings[h.app.bindingIndex(sent.Project)].ScopeOf() != settings.ProjectScopeLocal {
		t.Fatalf("scopes not persisted: %v", err)
	}
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", jsonOf(t, map[string]any{
		"node": "alice", "listen": "127.0.0.1:0", "handler": "none",
	}), h.tokenHdr()); code != http.StatusOK || strings.Contains(raw, `"error"`) {
		t.Fatalf("reload both project contexts: %d %s", code, raw)
	}
	var projects []ProjectView
	if code, raw := h.api(t, http.MethodGet, "projects", nil, &projects); code != http.StatusOK || len(projects) != 2 ||
		!h.app.projects[network.ID].n.OwnsChat(networkChat.ID) || !h.app.projects[sent.Project].n.OwnsChat(sent.Chat) {
		t.Fatalf("two chat scopes not restored: %d %s", code, raw)
	}
}

func TestExistingNetworkSeatDoesNotStartNewTurns(t *testing.T) {
	runner := &seatRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	var network ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Shared", "dir": t.TempDir()}, &network); code != http.StatusOK {
		t.Fatalf("network project: %d %s", code, raw)
	}
	n := h.app.projects[network.ID].n
	// AddSeat saves a seat record before attempting its setup turn. Its
	// failed start simulates a pre-existing network seat without changing its
	// stopped flag or deleting its pending queue.
	if _, err := n.AddSeat(node.SeatRequest{Provider: node.ProviderCodex}); err == nil {
		t.Fatal("network context started a seat")
	}
	seats := n.Seats()
	if len(seats) != 1 || seats[0].Stopped {
		t.Fatalf("old seat record changed: %+v", seats)
	}
	chat, err := n.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: "Old queued question", AuthorKind: node.AuthorHuman,
		AskSeats: []string{seats[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2200 * time.Millisecond) // covers one regular seat dispatch tick
	seats = n.Seats()
	if len(seats) != 1 || seats[0].Stopped || len(seats[0].Pending) != 1 || seats[0].Pending[0].ID != message.ID {
		t.Fatalf("network seat dispatched or was mutated: %+v", seats)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.specs) != 0 {
		t.Fatalf("network seat launched: %+v", runner.specs)
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
