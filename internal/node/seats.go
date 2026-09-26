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
//   - A message from a peer that replies to a seat's message goes to that
//     seat (receiveChat, seatsForIncoming), assigned to it so no other session
//     or the worker answers it too.
//   - A seat whose session is live gets its messages through the session's
//     hooks and wakes (unreadFor, claimLocked, Ack); one that is not is run by
//     the node (seatsDue): a headless turn resuming its session with the
//     messages as the prompt (WakePrompt). The turn claims them first (a hook
//     of the session registering meanwhile does not take them too), acknowledges
//     them when it succeeds and frees them when it fails.
//   - A session that is not live but open in the agent's app (its transcript
//     written since the node's last turn of it, seatOccupied) is not resumed:
//     two writers of one session. Its messages wait for its hooks (SeatBusy).
//   - A failed turn is tried again after seatRetry; after the last it waits
//     for a person (SeatNeedsHuman: a new message or Start).
//   - Agents asking each other form an automatic chain like any other
//     (inheritChain; a seat's message continues the one it was asked by):
//     past MaxAutoDepth a request waits for a person (Paused), so two seats
//     never talk forever.
//   - The setup turn of a new seat (its introduction alone) posts nothing: the
//     node refuses its sends (senderAgent).
//   - Stop ends a running turn and holds the seat's messages; Start resumes.

// Seat statuses (SeatView.Status).
const (
	SeatActive     = "active"      // its session is live and in a turn
	SeatIdle       = "idle"        // its session is live and waits
	SeatRunning    = "running"     // the node runs a turn of it
	SeatOffline    = "closed"      // no live session: the node runs it for a message
	SeatStopped    = "stopped"     // stopped by the person
	SeatBusy       = "busy"        // open in the agent's app: its messages wait for its hooks
	SeatNeedsHuman = "needs_human" // its turns kept failing: a new message or Start runs it again
)

// seatRetry are the waits before the automatic tries of a seat's failed turn.
var seatRetry = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

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
	// LastTurn is when the node's last turn of it ended (seatOccupied); Fails
	// its failed turns in a row, RetryAt when the next is tried (seatRetry).
	LastTurn time.Time `json:"last_turn,omitempty"`
	Fails    int       `json:"fails,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
}

// needsHuman reports whether s's turns failed past its automatic tries.
func (s *Seat) needsHuman() bool { return s.Fails > len(seatRetry) }

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
	// Error is why the node's last turn of it failed; it runs again at RetryAt,
	// for a new message or at Start.
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
	// run: the node's running turn per seat; errs: its last failure; busy:
	// its session is open in the agent's app (seatOccupied); quiet: its turn
	// is its setup alone, which posts nothing.
	run   map[string]context.CancelFunc
	errs  map[string]string
	busy  map[string]bool
	quiet map[string]bool
	// marks: per seat and message (seatKey), a hook's claim, the node's wake
	// of the seat's session with it (claimLocked) or the node's turn of it.
	marks map[string]seatMark
	// dirty: the last save failed; seatsDue saves again.
	dirty bool
}

type seatMark struct {
	at    time.Time
	wake  bool
	turn  bool
	token string
}

func seatKey(seat, id string) string { return seat + "/" + id }

func openSeats(dir string) (*seatStore, error) {
	st := &seatStore{path: filepath.Join(dir, "seats.json"), run: map[string]context.CancelFunc{}, errs: map[string]string{},
		busy: map[string]bool{}, quiet: map[string]bool{}, marks: map[string]seatMark{}}
	if err := readJSON(st.path, &st.seats); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return st, nil
}

// saveLocked writes seats.json; a failure leaves the store dirty, saved again
// by seatsDue, so what is in memory reaches the disk.
func (st *seatStore) saveLocked() error {
	if st.seats == nil {
		st.seats = []*Seat{}
	}
	err := writeJSON(st.path, st.seats)
	st.dirty = err != nil
	return err
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
		case s.needsHuman():
			v.Status = SeatNeedsHuman
		case st.busy[s.ID] && len(s.Pending) > 0:
			v.Status = SeatBusy
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
	s.Stopped, s.Fails, s.RetryAt = false, 0, time.Time{}
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
		if !n.startSeatTurn(seat.ID, true, open) {
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
	delete(st.busy, id)
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

// ErrSeatSetup: a seat's setup turn tried to post.
var ErrSeatSetup = errors.New("a seat's setup turn posts nothing")

// senderAgent is who sends req on this node: its seat (req.Seat, else the
// seat of req.SessionID) and provider; nil for a person or an unknown session.
// A seat in its setup turn may not send (ErrSeatSetup).
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
		if st.quiet[s.ID] {
			st.mu.Unlock()
			return nil, fmt.Errorf("%w: %w (%s)", ErrBadRequest, ErrSeatSetup, s.Label)
		}
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

// seatTo is a seat a message goes to: asked, else informed.
type seatTo struct {
	seat string
	ask  bool
}

// repliedSeat is the seat of this node that wrote the message id, "" for none.
func (n *Node) repliedSeat(id string) string {
	if id == "" {
		return ""
	}
	if r, ok := n.chats.message(id); ok && r.Message.From == n.cfg.Node && r.Message.Agent != nil {
		return r.Message.Agent.Seat
	}
	return ""
}

// deliverToSeats hands m, just sent by this node, to the seats it is for: the
// asked ones (m.AskSeats), and the author seat of the message m replies to
// (informed; asked when a person replies, see SendRequest). An error means
// the seats' queue is not saved yet (seatsDue saves it again).
func (n *Node) deliverToSeats(m Message, sender string) error {
	var list []seatTo
	for _, id := range m.AskSeats {
		list = append(list, seatTo{id, true})
	}
	if s := n.repliedSeat(m.ReplyTo); s != "" && s != sender && !slices.Contains(m.AskSeats, s) {
		list = append(list, seatTo{s, false})
	}
	_, err := n.queueSeats(m.ID, list)
	return err
}

// seatsForIncoming hands m, a new message from a peer, to the seat whose
// message it replies to: asked when it asks this node or a person wrote it,
// else informed. It returns that seat ("" for none). The seat answers it
// alone: pending for it, neither the worker (ClaimRun) nor another session
// (claimLocked) takes it, and once stored it is assigned to it
// (assignToSeat). A peer's AskSeats name its own seats, never this node's.
func (n *Node) seatsForIncoming(m Message) string {
	seat := n.repliedSeat(m.ReplyTo)
	if seat == "" || m.Kind != "" {
		return ""
	}
	queued, err := n.queueSeats(m.ID, []seatTo{{seat, m.AuthorKind == AuthorHuman || m.Asks(n.cfg.Node)}})
	if err != nil {
		n.log.Warn("save seats; saved again later", "id", m.ID, "err", err)
	}
	if !queued {
		return ""
	}
	return seat
}

// seatHas reports whether message id is pending for a seat of this node.
func (n *Node) seatHas(id string) bool {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, s := range st.seats {
		if slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == id }) {
			return true
		}
	}
	return false
}

// assignToSeat assigns stored message m to seat (seatsForIncoming; "" does
// nothing): read for this node's other sessions, its author told it is read.
func (n *Node) assignToSeat(m Message, seat string) {
	if seat == "" {
		return
	}
	ok, wasUnread, err := n.chats.claim(m.ID, "seat:"+seat)
	if err != nil {
		n.log.Warn("assign a message to its seat", "id", m.ID, "seat", seat, "err", err)
		return
	}
	if ok && wasUnread {
		n.sendReceipts(map[string][]string{m.From: {m.ID}}, StateRead)
		n.changed("messages")
	}
}

// queueSeats adds message id to the pending messages of the seats of list
// and saves them. queued reports whether any seat has it now; err that the
// save failed (the queue stays in memory and seatsDue saves it again).
func (n *Node) queueSeats(id string, list []seatTo) (queued bool, err error) {
	if len(list) == 0 {
		return false, nil
	}
	now := time.Now().UTC()
	st := n.seats
	st.mu.Lock()
	for _, t := range list {
		s := st.getLocked(t.seat)
		if s == nil {
			continue
		}
		queued = true
		if slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == id }) {
			continue
		}
		s.Pending = append(s.Pending, SeatPending{ID: id, Ask: t.ask, At: now})
		s.Fails, s.RetryAt = 0, time.Time{} // a new message: a failed seat runs again
	}
	if queued {
		err = st.saveLocked()
	}
	st.mu.Unlock()
	if queued {
		n.changed("seats")
	}
	return queued, err
}

// seatPaused reports whether a seat's pending message waits for a person: it
// asks, past the hop limit (MaxAutoDepth unless the project sets another).
func (n *Node) seatPaused(p SeatPending, m Message) bool { return p.Ask && n.overDepth(m) }

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
		token, woke, turn := st.held(seat, p.ID)
		switch {
		case turn:
			continue // the node's turn of the seat has it
		case woke:
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
	um.Paused = n.seatPaused(p, rec.Message)
	um.AsksYou = p.Ask && !um.Paused
	n.materialize(&um.Message, c.Area)
	return um, c.Area, true
}

// held reports whether message id of seat is held by the wake of its session
// (woke, with the wake's token, while it holds: inboxWakeGrace) or by the
// node's turn of it (turn).
func (st *seatStore) held(seat, id string) (token string, woke, turn bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.marks[seatKey(seat, id)]
	switch {
	case !ok:
	case m.turn:
		return "", false, true
	case m.wake && time.Since(m.at) < inboxWakeGrace:
		return m.token, true, false
	}
	return "", false, false
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
		case m.turn:
			return true, false // the node's turn of the seat has it
		case m.wake && time.Since(m.at) < inboxWakeGrace:
			return true, false // the wake prompt has it
		case wake && !m.wake && time.Since(m.at) < claimTTL:
			return true, false // a hook is delivering it
		}
	}
	st.marks[k] = seatMark{at: time.Now(), wake: wake, token: token}
	return true, true
}

// seatPendingFor is the seat of session when message id is pending for it
// (seatClaim handles it then), else "".
func (n *Node) seatPendingFor(session, id string) string {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.bySessionLocked(session)
	if s == nil || s.Stopped || !slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == id }) {
		return ""
	}
	return s.ID
}

// seatOfSession is the seat bound to session, or "".
func (n *Node) seatOfSession(session string) string {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.bySessionLocked(session); s != nil {
		return s.ID
	}
	return ""
}

// seatUnclaim drops the claims of the seat of session on ids (a failed wake).
func (n *Node) seatUnclaim(session string, ids []string) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.bySessionLocked(session); s != nil {
		for _, id := range ids {
			if k := seatKey(s.ID, id); !st.marks[k].turn {
				delete(st.marks, k)
			}
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
// running, not waiting to retry a failed turn or for a person, and its session
// not open in the agent's app (seatOccupied: SeatBusy, its hooks deliver).
// It also saves a seats.json whose last save failed.
func (n *Node) seatsDue(ctx context.Context, now time.Time) {
	dl, ok := n.launcher.(DirectLauncher)
	if !ok {
		return
	}
	live := n.sess.liveIDs(now)
	st := n.seats
	st.mu.Lock()
	if st.dirty {
		if err := st.saveLocked(); err != nil {
			n.log.Warn("save seats", "err", err)
		}
	}
	var due []Seat
	for _, s := range st.seats {
		if s.Stopped || st.run[s.ID] != nil || len(s.Pending) == 0 || (s.SessionID != "" && live[s.SessionID]) ||
			s.needsHuman() || (s.Fails > 0 && now.Before(s.RetryAt)) {
			continue
		}
		c := *s
		c.Pending = slices.Clone(s.Pending)
		due = append(due, c)
	}
	st.mu.Unlock()
	if !n.autoOK() {
		return // stopped, or paused by the budgets (autonomy.go)
	}
	for _, s := range due {
		ready := false
		for _, p := range s.Pending {
			if l, _ := n.leases.get(leaseKey(s.ID, p.ID)); l.Failed {
				continue // a person decides
			}
			if rec, ok := n.chats.message(p.ID); ok && !n.seatPaused(p, rec.Message) {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		busy := s.SessionID != "" && n.seatOccupied(s, now)
		st.mu.Lock()
		was := st.busy[s.ID]
		if busy {
			st.busy[s.ID] = true
		} else {
			delete(st.busy, s.ID)
		}
		st.mu.Unlock()
		if was != busy {
			n.changed("seats")
		}
		if busy {
			continue
		}
		if !n.autoTake(now) {
			return
		}
		id, intro := s.ID, s.SessionID == ""
		n.wg.Go(func() { n.seatTurn(ctx, dl, id, intro, false) })
	}
}

// startSeatTurn runs one turn of seat id in the background: its introduction
// first when intro, then its pending messages (as many as fit), resuming its
// session (a new one when it has none). It reports false when the node does
// not run or has no DirectLauncher.
func (n *Node) startSeatTurn(id string, intro, open bool) bool {
	dl, ok := n.launcher.(DirectLauncher)
	if !ok {
		return false
	}
	return n.spawn(func(ctx context.Context) { n.seatTurn(ctx, dl, id, intro, open) })
}

// seatTurn runs one turn of seat id unless one runs already or the seat is
// gone or stopped; StopSeat and RemoveSeat end it.
func (n *Node) seatTurn(ctx context.Context, dl DirectLauncher, id string, intro, open bool) {
	st := n.seats
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st.mu.Lock()
	s := st.getLocked(id)
	// Nothing runs while the node's agents are stopped (SetStopped).
	if s == nil || s.Stopped || st.run[id] != nil || n.Stopped() {
		st.mu.Unlock()
		return
	}
	st.run[id] = cancel
	seat := *s
	seat.Pending = slices.Clone(s.Pending)
	st.mu.Unlock()
	n.changed("seats")
	n.runSeatTurn(ctx, dl, seat, intro, open)
	st.mu.Lock()
	delete(st.run, id)
	delete(st.quiet, id)
	st.mu.Unlock()
	n.changed("seats")
}

// claimTurn claims msgs of seat for the node's turn of it (a hook or wake of
// its session does not take them meanwhile), leaving out the ones no longer
// pending or held by a hook's claim or a wake. ok is false when the seat is
// gone or stopped (or ctx done): no turn starts. A turn of the introduction
// alone is quiet: the seat's sends are refused (senderAgent).
func (n *Node) claimTurn(ctx context.Context, seat string, msgs []UnreadMessage, intro bool) (out []UnreadMessage, ok bool) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.getLocked(seat)
	if s == nil || s.Stopped || ctx.Err() != nil {
		return nil, false
	}
	now := time.Now()
	for _, m := range msgs {
		if !slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == m.ID }) {
			continue
		}
		k := seatKey(seat, m.ID)
		if mk, held := st.marks[k]; held && (mk.turn || (mk.wake && now.Sub(mk.at) < inboxWakeGrace) || (!mk.wake && now.Sub(mk.at) < claimTTL)) {
			continue
		}
		st.marks[k] = seatMark{at: now, turn: true}
		out = append(out, m)
	}
	if intro && len(out) == 0 {
		st.quiet[seat] = true
	}
	return out, true
}

// endTurn records the end of the node's turn of seat: its messages msgs leave
// the turn's claim (acknowledged by the caller when it succeeded), and a
// failure (err, not a stop) counts toward seatRetry.
func (n *Node) endTurn(seat string, msgs []UnreadMessage, err error, stopped bool) {
	st := n.seats
	st.mu.Lock()
	for _, m := range msgs {
		if k := seatKey(seat, m.ID); st.marks[k].turn {
			delete(st.marks, k)
		}
	}
	if err != nil {
		st.errs[seat] = trimMsg(err.Error())
	} else {
		delete(st.errs, seat)
	}
	var serr error
	if s := st.getLocked(seat); s != nil {
		s.LastTurn = time.Now().UTC()
		switch {
		case err == nil:
			s.Fails, s.RetryAt = 0, time.Time{}
		case !stopped:
			s.Fails++
			if !s.needsHuman() {
				s.RetryAt = time.Now().UTC().Add(seatRetry[s.Fails-1])
			}
		}
		serr = st.saveLocked()
	}
	st.mu.Unlock()
	if serr != nil {
		n.log.Warn("save seats", "err", serr)
	}
}

func (n *Node) runSeatTurn(ctx context.Context, dl DirectLauncher, seat Seat, intro, open bool) {
	folder := n.folders.work
	// A paused message (past MaxAutoDepth) waits for a person, never in a turn.
	var msgs []UnreadMessage
	for _, p := range seat.Pending {
		if um, _, ok := n.seatMessage(seat.ID, p); ok && !um.Paused {
			msgs = append(msgs, um)
		}
	}
	ready := len(msgs)
	if len(msgs) > 0 {
		msgs = msgs[:FitUnread(msgs)]
	}
	msgs, ok := n.claimTurn(ctx, seat.ID, msgs, intro)
	if !ok || (!intro && len(msgs) == 0) {
		n.endTurnClaims(seat.ID, msgs)
		return
	}
	owner, now := seatOwner(seat.ID), time.Now()
	var leased, refused []UnreadMessage
	for _, m := range msgs {
		// The seat keeps its own retry (seatRetry); a message whose lease is
		// failed (or acked) is not in the turn, like a launch's.
		took, err := n.leases.take(m.ID, seat.ID, owner, ViaSeat, "", now.Add(launchHold), now)
		if err != nil {
			n.log.Warn("save leases", "err", err)
		}
		if took {
			leased = append(leased, m)
		} else {
			refused = append(refused, m)
		}
	}
	msgs = leased
	if len(refused) > 0 {
		st := n.seats
		st.mu.Lock()
		for _, m := range refused {
			if k := seatKey(seat.ID, m.ID); st.marks[k].turn {
				delete(st.marks, k)
			}
		}
		if intro && len(msgs) == 0 {
			st.quiet[seat.ID] = true // its introduction alone posts nothing
		}
		st.mu.Unlock()
	}
	if !intro && len(msgs) == 0 {
		n.endTurnClaims(seat.ID, nil)
		return
	}
	chat := ""
	if len(msgs) > 0 {
		chat = msgs[0].ChatID
	} else if info, err := n.NewProjectChat(nil); err == nil {
		chat = info.ID
	}
	var prompt strings.Builder
	if intro {
		prompt.WriteString(n.seatIntro(seat, chat, len(msgs) == 0))
	}
	if len(msgs) > 0 {
		if prompt.Len() > 0 {
			prompt.WriteString("\n\n")
		}
		prompt.WriteString(WakePrompt(msgs, ready-len(msgs), folder, randomHex(8)))
	}
	spec := LaunchSpec{Provider: seat.Provider, Folder: folder, ResumeID: seat.SessionID, Prompt: prompt.String(),
		Seat: seat.ID, NoOpen: !open, Env: n.seatEnv(seat, chat)}
	n.log.Info("running a seat's turn", "seat", seat.ID, "provider", seat.Provider, "resume", seat.SessionID, "messages", len(msgs))
	err := dl.Run(ctx, spec, func(id string) {
		n.bindSeat(seat.ID, id)
		if _, err := n.leases.start(owner, "", "", ids(msgs), time.Now()); err != nil {
			n.log.Warn("save leases", "err", err)
		}
	})
	if errors.Is(err, ErrOpenApp) {
		err = nil
	}
	stopped := err != nil && ctx.Err() != nil
	if stopped {
		err = fmt.Errorf("stopped: %w", err)
	}
	if err != nil {
		n.log.Warn("a seat's turn failed", "seat", seat.ID, "err", err)
	}
	if err == nil && len(msgs) > 0 {
		// Acknowledged under the turn's claim: no hook takes them in between.
		done := n.seatAck("", seat.ID, ids(msgs))
		if err := n.leases.ack(seat.ID, nil, done, time.Now()); err != nil {
			n.log.Warn("save leases", "err", err)
		}
		n.changed("messages")
	} else if len(msgs) > 0 {
		n.revokeLeases(owner, ids(msgs), "seat_turn_failed", false)
	}
	n.endTurn(seat.ID, msgs, err, stopped)
}

// endTurnClaims drops the turn's claims of msgs of seat (a turn that did not
// start).
func (n *Node) endTurnClaims(seat string, msgs []UnreadMessage) {
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, m := range msgs {
		if k := seatKey(seat, m.ID); st.marks[k].turn {
			delete(st.marks, k)
		}
	}
	delete(st.quiet, seat)
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

// seatIntro is the first prompt of a seat's session; setup when it is the
// whole prompt (no message yet: the turn posts nothing).
func (n *Node) seatIntro(seat Seat, chat string, setup bool) string {
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
	intro := fmt.Sprintf("agent-link: вы — агент «%s» (%s) в разговоре проекта «%s» на машине %s, чат %s. "+
		"Другие локальные агенты этого разговора: %s. Сообщения вам приходят сами. "+
		"Спросить другого агента: agentlink send --chat %s --ask-seat <имя> --body \"<текст>\" "+
		"(список агентов: agentlink seats). Ответить на сообщение: agentlink send --chat %s --reply-to <id> --body \"<текст>\". "+
		"Никогда не пишите в чат по своей инициативе (ни приветствий, ни представлений): только ответ на сообщение, "+
		"которое просит ответа от вас, или вопрос, без которого эту работу не сделать. Ответ на ваш вопрос придёт сам.",
		seat.Label, ProviderName(seat.Provider), name, n.cfg.Node, chat, with, chat, chat)
	if setup {
		intro += " Сейчас сообщений нет: ничего не отправляйте и ничего не делайте, просто завершите ход."
	}
	return intro
}
