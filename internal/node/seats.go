package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Seats: the local agents (Claude Code, Codex) a person added to the project
// conversation of this node. A seat is one agent session in the project
// folder, shown as "<node> · <label>"; it is no network peer: peers see its
// messages as this node's, with Message.Agent naming it.
//
//   - The person adds a seat (AddSeat): the node opens a new session of its
//     provider in the folder (DirectLauncher, the desktop app's CLI) with an
//     introduction, and keeps its session id.
//   - A message asks seats by Message.AskSeats (the composer's selector, send
//     --ask-seat): each asked seat gets it (Seat.Pending), never another
//     session. A reply (ReplyTo) to a seat's message goes back to that seat: a
//     person's reply asks it, an agent's informs it.
//   - A seat whose session is live gets its messages through the session's
//     hooks and wakes (unreadFor, claimLocked, Ack); one that is not is run by
//     the node (seatsDue): a headless turn resuming its session with the
//     messages as the prompt (WakePrompt), acknowledged when the turn succeeds.
//   - Agents asking each other form an automatic chain like any other
//     (inheritChain): past MaxAutoDepth a request waits for a person (Paused),
//     so two seats never talk forever.
//   - Stop ends a running turn and holds the seat's messages; Start resumes.

// Seat statuses (SeatView.Status).
const (
	SeatActive  = "active"  // its session is live and in a turn
	SeatIdle    = "idle"    // its session is live and waits
	SeatRunning = "running" // the node runs a turn of it
	SeatOffline = "closed"  // no live session: the node runs it for a message
	SeatStopped = "stopped" // stopped by the person
)

// maxSeats bounds the seats of a node.
const maxSeats = 8

// ErrUnknownSeat: no seat with that id or label.
var ErrUnknownSeat = errors.New("unknown seat")

// Seat is one local agent of the project conversation.
type Seat struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Label     string    `json:"label"`
	SessionID string    `json:"session_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Stopped   bool      `json:"stopped,omitempty"`
	// Pending are the messages for it not yet acknowledged, oldest first.
	Pending []SeatPending `json:"pending,omitempty"`
}

// SeatPending is one message for a seat: Ask when it asks the seat to answer
// (else it informs, e.g. a reply to the seat's own message).
type SeatPending struct {
	ID  string    `json:"id"`
	Ask bool      `json:"ask,omitempty"`
	At  time.Time `json:"at"`
}

// SeatView is a seat as the app shows it.
type SeatView struct {
	Seat
	Status string `json:"status"`
	// Error is why the node's last turn of it failed; it runs again for a new
	// message or at Start.
	Error string `json:"error,omitempty"`
}

// SeatRequest is the body of adding a seat.
type SeatRequest struct {
	Provider string `json:"provider"`
	Label    string `json:"label,omitempty"`
	// Open shows the session in the agent's desktop app.
	Open bool `json:"open,omitempty"`
}

type seatStore struct {
	path string

	mu    sync.Mutex
	seats []*Seat
	// run: the node's running turn per seat; errs: its last failure;
	// blocked: a failed turn waits for a new message or Start.
	run     map[string]context.CancelFunc
	errs    map[string]string
	blocked map[string]bool
	// marks: per seat and message (seatKey), a hook's claim or the node's
	// wake of the seat's session with it (claimLocked).
	marks map[string]seatMark
}

type seatMark struct {
	at    time.Time
	wake  bool
	token string
}

func seatKey(seat, id string) string { return seat + "/" + id }

func openSeats(dir string) (*seatStore, error) {
	st := &seatStore{path: filepath.Join(dir, "seats.json"), run: map[string]context.CancelFunc{}, errs: map[string]string{},
		blocked: map[string]bool{}, marks: map[string]seatMark{}}
	if err := readJSON(st.path, &st.seats); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return st, nil
}

func (st *seatStore) saveLocked() error {
	if st.seats == nil {
		st.seats = []*Seat{}
	}
	return writeJSON(st.path, st.seats)
}

func (st *seatStore) getLocked(id string) *Seat {
	for _, s := range st.seats {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// bySessionLocked is the seat of live session sid, nil for none.
func (st *seatStore) bySessionLocked(sid string) *Seat {
	if sid == "" {
		return nil
	}
	for _, s := range st.seats {
		if s.SessionID == sid {
			return s
		}
	}
	return nil
}

// Seats lists this node's seats with their status.
func (n *Node) Seats() []SeatView {
	now := time.Now()
	type live struct{ ok, idle bool }
	sess := map[string]live{}
	n.sess.mu.Lock()
	for id, s := range n.sess.sessions {
		if s.live(now) {
			sess[id] = live{true, s.Idle}
		}
	}
	n.sess.mu.Unlock()
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]SeatView, 0, len(st.seats))
	for _, s := range st.seats {
		c := *s
		c.Pending = slices.Clone(s.Pending)
		v := SeatView{Seat: c, Error: st.errs[s.ID]}
		l := sess[s.SessionID]
		switch {
		case s.Stopped:
			v.Status = SeatStopped
		case st.run[s.ID] != nil:
			v.Status = SeatRunning
		case l.ok && l.idle:
			v.Status = SeatIdle
		case l.ok:
			v.Status = SeatActive
		default:
			v.Status = SeatOffline
		}
		out = append(out, v)
	}
	return out
}

// seatLabel is label cleaned, or the provider's name (numbered when taken).
func seatLabel(label, provider string, taken func(string) bool) (string, error) {
	label = strings.TrimSpace(label)
	if label != "" {
		if utf8.RuneCountInString(label) > 32 || strings.ContainsAny(label, ",\n\r\t@") {
			return "", fmt.Errorf("%w: invalid label", ErrBadRequest)
		}
		if taken(label) {
			return "", fmt.Errorf("%w: label %q taken", ErrBadRequest, label)
		}
		return label, nil
	}
	base := ProviderName(provider)
	for i := 1; ; i++ {
		l := base
		if i > 1 {
			l = fmt.Sprintf("%s %d", base, i)
		}
		if !taken(l) {
			return l, nil
		}
	}
}

// ProviderName is how a provider is shown: Claude, Codex.
func ProviderName(p string) string {
	switch p {
	case ProviderClaude:
		return "Claude"
	case ProviderCodex:
		return "Codex"
	}
	return p
}

// AddSeat adds a seat of req.Provider and starts its session (StartSeat).
func (n *Node) AddSeat(req SeatRequest) (SeatView, error) {
	switch {
	case n.cfg.Project == "":
		return SeatView{}, ErrNotProject
	case n.NeedsFolder():
		return SeatView{}, ErrNeedsFolder
	case req.Provider != ProviderClaude && req.Provider != ProviderCodex:
		return SeatView{}, fmt.Errorf("%w: provider must be %q or %q", ErrBadRequest, ProviderClaude, ProviderCodex)
	}
	st := n.seats
	st.mu.Lock()
	if len(st.seats) >= maxSeats {
		st.mu.Unlock()
		return SeatView{}, fmt.Errorf("%w: at most %d seats", ErrBadRequest, maxSeats)
	}
	label, err := seatLabel(req.Label, req.Provider, func(l string) bool {
		return slices.ContainsFunc(st.seats, func(s *Seat) bool { return strings.EqualFold(s.Label, l) || s.ID == l })
	})
	if err != nil {
		st.mu.Unlock()
		return SeatView{}, err
	}
	s := &Seat{ID: "seat-" + randomHex(4), Provider: req.Provider, Label: label, CreatedAt: time.Now().UTC()}
	st.seats = append(st.seats, s)
	err = st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		return SeatView{}, err
	}
	n.log.Info("seat added", "seat", s.ID, "provider", s.Provider, "label", s.Label)
	n.changed("seats")
	return n.StartSeat(s.ID, req.Open)
}

// StartSeat lets seat id run again (after StopSeat or a failed turn). A seat
// without a session gets one: a turn with its introduction (and its pending
// messages). open shows the session in the desktop app.
func (n *Node) StartSeat(id string, open bool) (SeatView, error) {
	st := n.seats
	st.mu.Lock()
	s := st.getLocked(id)
	if s == nil {
		st.mu.Unlock()
		return SeatView{}, fmt.Errorf("%w %s", ErrUnknownSeat, id)
	}
	s.Stopped = false
	delete(st.blocked, id)
	delete(st.errs, id)
	err := st.saveLocked()
	seat, running := *s, st.run[id] != nil
	st.mu.Unlock()
	if err != nil {
		return SeatView{}, err
	}
	switch {
	case running:
	case seat.SessionID == "":
		if !n.startSeatTurn(seat, true, open) {
			return SeatView{}, fmt.Errorf("%w: the node is not running", ErrBadRequest)
		}
	case open:
		if err := openURL(context.Background(), DeepLink(seat.Provider, seat.SessionID)); err != nil {
			n.log.Warn("open seat session", "seat", id, "err", err)
		}
	}
	n.changed("seats")
	return n.seatView(id)
}

// StopSeat stops seat id: its running turn ends, its messages wait (StartSeat).
func (n *Node) StopSeat(id string) (SeatView, error) {
	st := n.seats
	st.mu.Lock()
	s := st.getLocked(id)
	if s == nil {
		st.mu.Unlock()
		return SeatView{}, fmt.Errorf("%w %s", ErrUnknownSeat, id)
	}
	s.Stopped = true
	if cancel := st.run[id]; cancel != nil {
		cancel()
	}
	err := st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		return SeatView{}, err
	}
	n.changed("seats")
	return n.seatView(id)
}

// RemoveSeat stops seat id and forgets it; its session stays the person's.
func (n *Node) RemoveSeat(id string) error {
	st := n.seats
	st.mu.Lock()
	i := slices.IndexFunc(st.seats, func(s *Seat) bool { return s.ID == id })
	if i < 0 {
		st.mu.Unlock()
		return fmt.Errorf("%w %s", ErrUnknownSeat, id)
	}
	if cancel := st.run[id]; cancel != nil {
		cancel()
	}
	st.seats = slices.Delete(st.seats, i, i+1)
	delete(st.errs, id)
	delete(st.blocked, id)
	for k := range st.marks {
		if strings.HasPrefix(k, id+"/") {
			delete(st.marks, k)
		}
	}
	err := st.saveLocked()
	st.mu.Unlock()
	n.changed("seats")
	return err
}

func (n *Node) seatView(id string) (SeatView, error) {
	for _, v := range n.Seats() {
		if v.ID == id {
			return v, nil
		}
	}
	return SeatView{}, fmt.Errorf("%w %s", ErrUnknownSeat, id)
}

// senderAgent is who sends req on this node: its seat (req.Seat, else the
// seat of req.SessionID) and provider; nil for a person or an unknown session.
func (n *Node) senderAgent(req SendRequest) (*AgentRef, error) {
	if req.AuthorKind == AuthorHuman {
		return nil, nil
	}
	st := n.seats
	st.mu.Lock()
	var s *Seat
	if req.Seat != "" {
		if s = st.getLocked(req.Seat); s == nil {
			st.mu.Unlock()
			return nil, fmt.Errorf("%w %s", ErrUnknownSeat, req.Seat)
		}
	} else {
		s = st.bySessionLocked(req.SessionID)
	}
	var ref *AgentRef
	if s != nil {
		ref = &AgentRef{Seat: s.ID, Label: s.Label, Provider: s.Provider}
	}
	st.mu.Unlock()
	if ref != nil || req.SessionID == "" {
		return ref, nil
	}
	n.sess.mu.Lock()
	defer n.sess.mu.Unlock()
	if ss := n.sess.sessions[req.SessionID]; ss != nil && ss.live(time.Now()) {
		return &AgentRef{Provider: ss.Provider}, nil
	}
	return nil, nil
}

// resolveSeats maps names (seat ids or labels, also comma-separated; "all"
// for every seat) to seat ids, leaving out except (the sender's own seat).
func (n *Node) resolveSeats(names []string, except string) ([]string, error) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, name := range splitNames(names) {
		name = strings.TrimPrefix(name, "@")
		if strings.EqualFold(name, "all") || name == "*" {
			for _, s := range st.seats {
				out = append(out, s.ID)
			}
			continue
		}
		i := slices.IndexFunc(st.seats, func(s *Seat) bool { return s.ID == name || strings.EqualFold(s.Label, name) })
		if i < 0 {
			return nil, fmt.Errorf("%w: %w %q", ErrBadRequest, ErrUnknownSeat, name)
		}
		out = append(out, st.seats[i].ID)
	}
	out = slices.DeleteFunc(out, func(id string) bool { return id == except })
	slices.Sort(out)
	return slices.Compact(out), nil
}

// deliverToSeats hands m, just sent by this node, to the seats it is for: the
// asked ones (m.AskSeats), and the author seat of the message m replies to
// (informed; asked when a person replies, see SendRequest).
func (n *Node) deliverToSeats(m Message, sender string) {
	type to struct {
		seat string
		ask  bool
	}
	var list []to
	for _, id := range m.AskSeats {
		list = append(list, to{id, true})
	}
	if m.ReplyTo != "" {
		if r, ok := n.chats.message(m.ReplyTo); ok && r.Message.From == n.cfg.Node && r.Message.Agent != nil {
			if s := r.Message.Agent.Seat; s != "" && s != sender && !slices.Contains(m.AskSeats, s) {
				list = append(list, to{s, false})
			}
		}
	}
	if len(list) == 0 {
		return
	}
	now := time.Now().UTC()
	st := n.seats
	st.mu.Lock()
	for _, t := range list {
		s := st.getLocked(t.seat)
		if s == nil || slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == m.ID }) {
			continue
		}
		s.Pending = append(s.Pending, SeatPending{ID: m.ID, Ask: t.ask, At: now})
		delete(st.blocked, s.ID) // a new message: a failed seat runs again
	}
	err := st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		n.log.Warn("save seats", "err", err)
	}
	n.changed("seats")
}

// seatPaused reports whether a seat's pending message waits for a person: it
// asks, past MaxAutoDepth.
func seatPaused(p SeatPending, m Message) bool { return p.Ask && m.AutoDepth > MaxAutoDepth }

// seatUnread is the unread messages of the seat of live session sid (as
// unreadFor lists them): the ones its wake prompt carries apart (woken).
func (n *Node) seatUnread(sid string, filter bool, area string, actionable bool) (msgs, woken []UnreadMessage) {
	st := n.seats
	st.mu.Lock()
	s := st.bySessionLocked(sid)
	if s == nil || s.Stopped {
		st.mu.Unlock()
		return nil, nil
	}
	seat, pend := s.ID, slices.Clone(s.Pending)
	st.mu.Unlock()
	for _, p := range pend {
		um, chatArea, ok := n.seatMessage(seat, p)
		if !ok || (filter && n.localArea(chatArea) != area) || (actionable && um.Paused) {
			continue
		}
		if token, ok := st.woke(seat, p.ID); ok {
			um.WakeToken = token
			woken = append(woken, um)
			continue
		}
		msgs = append(msgs, um)
	}
	return msgs, woken
}

// seatMessage is pending message p of seat as its session reads it (an
// UnreadMessage for it alone), with the area of its chat.
func (n *Node) seatMessage(seat string, p SeatPending) (UnreadMessage, string, bool) {
	rec, ok := n.chats.message(p.ID)
	if !ok {
		return UnreadMessage{}, "", false
	}
	c, ok := n.chats.get(rec.Message.ChatID)
	if !ok {
		return UnreadMessage{}, "", false
	}
	um := UnreadMessage{ChatMessage: n.chatMessage(c, rec), ReceivedAt: p.At, ForSeat: seat}
	um.Direction, um.Unread, um.OwnHuman, um.Delivery = "in", true, false, nil
	um.Paused = seatPaused(p, rec.Message)
	um.AsksYou = p.Ask && !um.Paused
	n.materialize(&um.Message, c.Area)
	return um, c.Area, true
}

// woke reports the token of the wake of seat's session with message id, while
// it holds (inboxWakeGrace).
func (st *seatStore) woke(seat, id string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.marks[seatKey(seat, id)]
	if ok && m.wake && time.Since(m.at) < inboxWakeGrace {
		return m.token, true
	}
	return "", false
}

// seatClaim is claimLocked for a message pending for the seat of session:
// handled is false when it is none; granted like claimLocked (a wake never
// takes what a hook claimed, a hook never what a wake holds).
func (n *Node) seatClaim(session, id string, wake bool, token string) (handled, granted bool) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.bySessionLocked(session)
	if s == nil || s.Stopped || !slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == id }) {
		return false, false
	}
	k := seatKey(s.ID, id)
	if m, ok := st.marks[k]; ok {
		switch {
		case m.wake && time.Since(m.at) < inboxWakeGrace:
			return true, false // the wake prompt has it
		case wake && !m.wake && time.Since(m.at) < claimTTL:
			return true, false // a hook is delivering it
		}
	}
	st.marks[k] = seatMark{at: time.Now(), wake: wake, token: token}
	return true, true
}

// seatUnclaim drops the claims of the seat of session on ids (a failed wake).
func (n *Node) seatUnclaim(session string, ids []string) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.bySessionLocked(session); s != nil {
		for _, id := range ids {
			delete(st.marks, seatKey(s.ID, id))
		}
	}
}

// seatAck acknowledges ids for the seat of session (or seat, when set): they
// leave its pending messages. It returns the ones that did.
func (n *Node) seatAck(session, seat string, ids []string) map[string]bool {
	st := n.seats
	st.mu.Lock()
	var s *Seat
	if seat != "" {
		s = st.getLocked(seat)
	} else {
		s = st.bySessionLocked(session)
	}
	if s == nil {
		st.mu.Unlock()
		return nil
	}
	done := map[string]bool{}
	s.Pending = slices.DeleteFunc(s.Pending, func(p SeatPending) bool {
		if slices.Contains(ids, p.ID) {
			done[p.ID] = true
			delete(st.marks, seatKey(s.ID, p.ID))
			return true
		}
		return false
	})
	var err error
	if len(done) > 0 {
		err = st.saveLocked()
	}
	st.mu.Unlock()
	if err != nil {
		n.log.Warn("save seats", "err", err)
	}
	if len(done) > 0 {
		n.changed("seats")
	}
	return done
}

// seatsDue starts a turn of every seat that has messages to answer and no
// live session to take them (its hooks and wakes do then): not stopped, not
// running, not blocked by a failed turn.
func (n *Node) seatsDue(ctx context.Context, now time.Time) {
	dl, ok := n.launcher.(DirectLauncher)
	if !ok {
		return
	}
	live := n.sess.liveIDs(now)
	st := n.seats
	st.mu.Lock()
	var due []Seat
	for _, s := range st.seats {
		if s.Stopped || st.run[s.ID] != nil || st.blocked[s.ID] || len(s.Pending) == 0 || (s.SessionID != "" && live[s.SessionID]) {
			continue
		}
		due = append(due, *s)
	}
	st.mu.Unlock()
	for _, s := range due {
		ready := false
		for _, p := range s.Pending {
			if rec, ok := n.chats.message(p.ID); ok && !seatPaused(p, rec.Message) {
				ready = true
				break
			}
		}
		if ready {
			n.wg.Go(func() { n.seatTurn(ctx, dl, s, s.SessionID == "", false) })
		}
	}
}

// startSeatTurn runs one turn of seat in the background: its introduction
// first when intro, then its pending messages (as many as fit), resuming its
// session (a new one when it has none). It reports false when the node does
// not run or has no DirectLauncher.
func (n *Node) startSeatTurn(seat Seat, intro, open bool) bool {
	dl, ok := n.launcher.(DirectLauncher)
	if !ok {
		return false
	}
	return n.spawn(func(ctx context.Context) { n.seatTurn(ctx, dl, seat, intro, open) })
}

// seatTurn runs one turn of seat unless one runs already; StopSeat ends it.
func (n *Node) seatTurn(ctx context.Context, dl DirectLauncher, seat Seat, intro, open bool) {
	st := n.seats
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st.mu.Lock()
	if st.run[seat.ID] != nil {
		st.mu.Unlock()
		return
	}
	st.run[seat.ID] = cancel
	st.mu.Unlock()
	n.changed("seats")
	n.runSeatTurn(ctx, dl, seat, intro, open)
	st.mu.Lock()
	delete(st.run, seat.ID)
	st.mu.Unlock()
	n.changed("seats")
}

func (n *Node) runSeatTurn(ctx context.Context, dl DirectLauncher, seat Seat, intro, open bool) {
	folder := n.folders.work
	var msgs []UnreadMessage
	for _, p := range seat.Pending {
		if um, _, ok := n.seatMessage(seat.ID, p); ok {
			msgs = append(msgs, um)
		}
	}
	if len(msgs) > 0 {
		msgs = msgs[:FitUnread(msgs)]
	}
	chat := ""
	if len(msgs) > 0 {
		chat = msgs[0].ChatID
	} else if info, err := n.NewProjectChat(nil); err == nil {
		chat = info.ID
	}
	var prompt strings.Builder
	if intro {
		prompt.WriteString(n.seatIntro(seat, chat))
	}
	if len(msgs) > 0 {
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		prompt.WriteString(WakePrompt(msgs, len(seat.Pending)-len(msgs), folder, randomHex(8)))
	}
	if prompt.Len() == 0 {
		return
	}
	spec := LaunchSpec{Provider: seat.Provider, Folder: folder, ResumeID: seat.SessionID, Prompt: prompt.String(),
		Seat: seat.ID, NoOpen: !open, Env: n.seatEnv(seat, chat)}
	n.log.Info("running a seat's turn", "seat", seat.ID, "provider", seat.Provider, "resume", seat.SessionID, "messages", len(msgs))
	err := dl.Run(ctx, spec, func(id string) { n.bindSeat(seat.ID, id) })
	st := n.seats
	if err != nil && !errors.Is(err, ErrOpenApp) {
		if ctx.Err() != nil {
			err = fmt.Errorf("stopped: %w", err)
		}
		n.log.Warn("a seat's turn failed", "seat", seat.ID, "err", err)
		st.mu.Lock()
		st.errs[seat.ID] = trimMsg(err.Error())
		st.blocked[seat.ID] = true
		st.mu.Unlock()
		return
	}
	st.mu.Lock()
	delete(st.errs, seat.ID)
	st.mu.Unlock()
	if len(msgs) > 0 {
		n.seatAck("", seat.ID, ids(msgs))
		n.changed("messages")
	}
}

// bindSeat keeps session id as seat's session.
func (n *Node) bindSeat(seat, id string) {
	st := n.seats
	st.mu.Lock()
	s := st.getLocked(seat)
	changed := s != nil && s.SessionID != id
	if changed {
		s.SessionID = id
		if err := st.saveLocked(); err != nil {
			n.log.Warn("save seats", "err", err)
		}
	}
	st.mu.Unlock()
	if changed {
		n.log.Info("seat session", "seat", seat, "session", id)
		n.changed("seats")
	}
}

// SetSeatEnv sets extra environment for the seats' turns (the e2e test points
// the agents' agentlink at its node).
func (n *Node) SetSeatEnv(env []string) { n.seatExtra = env }

// seatEnv is the environment of a seat's turn: its seat, project and chat for
// agentlink send, and this program's folder first on PATH.
func (n *Node) seatEnv(seat Seat, chat string) []string {
	env := []string{"AGENTLINK_SEAT=" + seat.ID, "AGENTLINK_PROJECT_ID=" + n.cfg.Project}
	if chat != "" {
		env = append(env, "AGENTLINK_CHAT_ID="+chat)
	}
	if exe, err := os.Executable(); err == nil && strings.HasPrefix(strings.ToLower(filepath.Base(exe)), "agentlink") {
		env = append(env, "PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return append(env, n.seatExtra...)
}

// seatIntro is the first prompt of a seat's session.
func (n *Node) seatIntro(seat Seat, chat string) string {
	var others []string
	st := n.seats
	st.mu.Lock()
	for _, s := range st.seats {
		if s.ID != seat.ID {
			others = append(others, fmt.Sprintf("%s (%s)", s.Label, ProviderName(s.Provider)))
		}
	}
	st.mu.Unlock()
	with := "пока никого"
	if len(others) > 0 {
		with = strings.Join(others, ", ")
	}
	name := n.ProjectMeta().Name
	return fmt.Sprintf("agent-link: вы — агент «%s» (%s) в разговоре проекта «%s» на машине %s, чат %s. "+
		"Другие локальные агенты этого разговора: %s. Сообщения вам приходят сами. "+
		"Спросить другого агента: agentlink send --chat %s --ask-seat <имя> --body \"<текст>\" "+
		"(список агентов: agentlink seats). Ответить на сообщение: agentlink send --chat %s --reply-to <id> --body \"<текст>\". "+
		"Отвечайте только когда вас просят; ответ на ваш вопрос придёт сам.",
		seat.Label, ProviderName(seat.Provider), name, n.cfg.Node, chat, with, chat, chat)
}
