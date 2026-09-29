package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// failRunner fails every seat turn with err, like a provider past its usage
// limit.
type failRunner struct{ err error }

func (*failRunner) Launch(context.Context, node.LaunchSpec) error { return nil }
func (*failRunner) Direct(string) bool                            { return true }
func (r *failRunner) Run(context.Context, node.LaunchSpec, func(string)) error {
	return r.err
}

func discussPost(t *testing.T, h *harness, body map[string]string) (int, discussResultAPI, string) {
	t.Helper()
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, body), nil)
	var got discussResultAPI
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
	}
	return code, got, raw
}

func discussReplyPath(r discussResultAPI, timeout string) string {
	return "/discuss/reply?" + url.Values{"project": {r.Project}, "chat": {r.Chat}, "id": {r.ID}, "seat": {r.Seat}, "timeout": {timeout}}.Encode()
}

// A seat whose turns fail (usage limit, or a retry later than the wait) ends
// the caller's wait at once with the reason instead of a silent timeout; the
// question stays pending for the seat.
func TestDiscussReplyReportsFailedSeat(t *testing.T) {
	for _, tc := range []struct {
		name, err, timeout string
	}{
		{"usage limit", "codex: You've hit your usage limit. Upgrade or try again at Oct 3rd, 2026 9:00 AM.", "10m"},
		{"retry after the wait", "exit status 1", "20s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &failRunner{err: errors.New(tc.err)} })
			code, ask, raw := discussPost(t, h, map[string]string{"folder": repoDir(t), "provider": node.ProviderCodex, "body": "question"})
			if code != http.StatusOK {
				t.Fatalf("discuss: %d %s", code, raw)
			}
			start := time.Now()
			code, body := h.do(t, http.MethodGet, discussReplyPath(ask, tc.timeout), "", nil)
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("failed seat answered after %v", took)
			}
			var held struct {
				Error      string    `json:"error"`
				HoldReason string    `json:"hold_reason"`
				SeatError  string    `json:"seat_error"`
				RetryAt    time.Time `json:"retry_at"`
			}
			if code != http.StatusConflict || json.Unmarshal([]byte(body), &held) != nil || held.HoldReason != node.HoldSeatFailed ||
				held.SeatError == "" || !strings.Contains(held.Error, held.SeatError) || !strings.Contains(tc.err, strings.TrimSuffix(held.SeatError, "…")[:10]) {
				t.Fatalf("failed seat wait: %d %s", code, body)
			}
			seats := h.app.projects[ask.Project].n.Seats()
			if len(seats) != 1 || len(seats[0].Pending) != 1 || seats[0].Pending[0].ID != ask.ID || seats[0].Fails == 0 {
				t.Fatalf("question not kept for the seat: %+v", seats)
			}
		})
	}
}

// A seat asking its own provider asks another seat of it (a new one), never
// itself: the message asks that seat and the wait for it is valid.
func TestDiscussSeatSelfAskUsesAnotherSeat(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := repoDir(t)
	code, codex, raw := discussPost(t, h, map[string]string{"folder": dir, "provider": node.ProviderCodex, "body": "setup"})
	if code != http.StatusOK {
		t.Fatalf("discuss setup: %d %s", code, raw)
	}
	n := h.app.projects[codex.Project].n
	eventuallyApp(t, "seat setup", func() bool {
		s := n.Seats()
		return len(s) == 1 && s[0].SessionID != "" && !s[0].LastTurn.IsZero() && s[0].Status != node.SeatRunning
	})
	code, self, raw := discussPost(t, h, map[string]string{"folder": dir, "provider": node.ProviderCodex, "seat": codex.Seat, "body": "Ask another Codex"})
	if code != http.StatusOK || self.Seat == "" || self.Seat == codex.Seat {
		t.Fatalf("seat asked itself: %d %s", code, raw)
	}
	msgs, err := n.ChatMessages(self.Chat, 0, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.ID == self.ID && (len(m.AskSeats) != 1 || m.AskSeats[0] != self.Seat || m.Agent == nil || m.Agent.Seat != codex.Seat) {
			t.Fatalf("self ask message: %+v", m.Message)
		}
	}
	if code, body := h.do(t, http.MethodGet, discussReplyPath(self, "30ms"), "", nil); code == http.StatusNotFound {
		t.Fatalf("wait for the other seat: %d %s", code, body)
	}
}

// chainRunner plays seats that, asked a "hop" question, ask a seat of the
// other provider in turn, wait for its answer inside their turn and then
// answer their own asker: codex → claude → codex → … until the hop limit.
type chainRunner struct {
	h   *harness
	t   *testing.T
	dir string

	mu   sync.Mutex
	hops []chainHop
}

type chainHop struct {
	from, to, hold string
	code           int
	wait           int
}

func (*chainRunner) Launch(context.Context, node.LaunchSpec) error { return nil }
func (*chainRunner) Direct(string) bool                            { return true }

func (r *chainRunner) Run(ctx context.Context, spec node.LaunchSpec, started func(string)) error {
	started(cmpOr(spec.ResumeID, "session-"+spec.Seat))
	if !strings.Contains(spec.Prompt, "hop") {
		return nil
	}
	var n *node.Node
	r.h.app.mu.Lock()
	for _, c := range r.h.app.projects {
		n = c.n
	}
	r.h.app.mu.Unlock()
	var askID, chat string
	for _, s := range n.Seats() {
		if s.ID == spec.Seat {
			for _, p := range s.Pending {
				if p.Ask {
					askID = p.ID
				}
			}
		}
	}
	other := node.ProviderClaude
	if spec.Provider == node.ProviderClaude {
		other = node.ProviderCodex
	}
	body := jsonOf(r.t, map[string]string{"folder": r.dir, "provider": other, "seat": spec.Seat, "body": "hop"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.h.srv.URL+"/discuss", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	hop := chainHop{from: spec.Seat}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		var got struct {
			discussResultAPI
			HoldReason string `json:"hold_reason"`
		}
		hop.code = resp.StatusCode
		_ = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		hop.to, hop.hold, chat = got.Seat, got.HoldReason, got.Chat
		if resp.StatusCode == http.StatusOK && got.HoldReason == "" {
			wreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.h.srv.URL+discussReplyPath(got.discussResultAPI, "20s"), nil)
			if wresp, err := http.DefaultClient.Do(wreq); err == nil {
				hop.wait = wresp.StatusCode
				_, _ = io.Copy(io.Discard, wresp.Body)
				_ = wresp.Body.Close()
			}
		}
	}
	r.mu.Lock()
	r.hops = append(r.hops, hop)
	r.mu.Unlock()
	if askID != "" && chat != "" {
		if _, err := n.SendRequest(node.SendRequest{ChatID: chat, ReplyTo: askID, Body: "answer", Seat: spec.Seat, AuthorKind: node.AuthorAgent}); err != nil {
			r.t.Errorf("answer %s: %v", askID, err)
		}
	}
	return nil
}

// Seats asking each other in a chain never ask a seat waiting upstream (that
// would wait until the timeout): each hop gets a fresh seat, the chain runs
// past the turn cap (a waiting turn lends its slot) and stops at the hop limit
// with a visible hold, and every wait ends with an answer.
func TestDiscussChainStopsAtHopLimit(t *testing.T) {
	runner := &chainRunner{t: t, dir: repoDir(t)}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	runner.h = h
	code, setup, raw := discussPost(t, h, map[string]string{"folder": runner.dir, "provider": node.ProviderCodex, "body": "setup"})
	if code != http.StatusOK {
		t.Fatalf("discuss setup: %d %s", code, raw)
	}
	n := h.app.projects[setup.Project].n
	cfg := n.Autonomy()
	cfg.MaxDepth = 5
	n.SetAutonomy(cfg)
	eventuallyApp(t, "seat setup", func() bool {
		s := n.Seats()
		return len(s) == 1 && !s[0].LastTurn.IsZero() && s[0].Status != node.SeatRunning
	})
	code, ask, raw := discussPost(t, h, map[string]string{"folder": runner.dir, "provider": node.ProviderCodex, "body": "hop"})
	if code != http.StatusOK || ask.Seat != setup.Seat {
		t.Fatalf("discuss: %d %s", code, raw)
	}
	start := time.Now()
	code, body := h.do(t, http.MethodGet, discussReplyPath(ask, "30s"), "", nil)
	if code != http.StatusOK || time.Since(start) > 15*time.Second {
		t.Fatalf("chain answer: %d %s after %v", code, body, time.Since(start))
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	// Hops are recorded as they end (deepest first); a seat waiting upstream
	// asked again would show up as a repeated target or the first seat.
	held, asked := 0, map[string]bool{ask.Seat: true}
	for _, hop := range runner.hops {
		switch {
		case hop.code != http.StatusOK:
			t.Fatalf("hop failed: %+v", hop)
		case hop.hold == node.HoldAutoLimit:
			held++
		case hop.to == hop.from || asked[hop.to] || hop.wait != http.StatusOK:
			t.Fatalf("hop asked a waiting seat or got no answer: %+v in %+v", hop, runner.hops)
		default:
			asked[hop.to] = true
		}
	}
	if held != 1 || len(runner.hops) != cfg.MaxDepth+1 {
		t.Fatalf("chain did not stop at the hop limit once: %+v", runner.hops)
	}
}
