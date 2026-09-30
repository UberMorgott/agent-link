package node

import (
	"cmp"
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
//   - A message asks seats by Message.AskSeats (send --ask-seat, discuss): each asked seat gets it (Seat.Pending), never another
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
	SeatNeedsHuman = "needs_human" // its turns kept failing (a new message or Start runs it again), or every message's lease failed (Start runs them again)
	SeatPaused     = "paused"      // every message for it is past the hop limit: it waits for a person
)

// seatRetry are the waits before the automatic tries of a seat's failed turn.
var seatRetry = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// maxSeats bounds the seats of a node.
const maxSeats = 8

// ErrUnknownSeat: no seat with that id or label.
var ErrUnknownSeat = errors.New("unknown seat")

// ErrSeatsDisabled: the project has no seats (config.DisableSeats: a network
// project, whose agents are the sessions its members open).
var ErrSeatsDisabled = errors.New("seats are off in this project")

// ErrSeatLimit: the node has maxSeats seats already.
var ErrSeatLimit = errors.New("seat limit reached")

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
	// TurnQueued: its turn (Status running) waits for a slot of the node's
	// turn cap (TurnGate).
	TurnQueued bool `json:"turn_queued,omitempty"`
}

// SeatRequest is the body of adding a seat.
type SeatRequest struct {
	Provider string `json:"provider"`
	Label    string `json:"label,omitempty"`
	// Open shows the session in the agent's desktop app.
	Open bool `json:"open,omitempty"`
	// Defer adds the seat without starting its first turn: the caller queues
	// a message for it first and then calls StartSeat, so the first turn has it.
	Defer bool `json:"-"`
}

type seatStore struct {
	path string

	mu    sync.Mutex
	seats []*Seat
	// run: the node's running turn per seat; errs: its last failure; busy:
	// its session is open in the agent's app (seatOccupied); quiet: its turn
	// is its setup alone, which posts nothing; deferred: added with Defer and not
	// started yet, so seatsDue leaves it to StartSeat.
	run      map[string]context.CancelFunc
	queued   map[string]bool // a turn in run waits for a TurnGate slot
	errs     map[string]string
	busy     map[string]bool
	quiet    map[string]bool
	deferred map[string]bool
	// marks: per seat and message (seatKey), a hook's claim, the node's wake
	// of the seat's session with it (claimLocked) or the node's turn of it.
	marks map[string]seatMark
	// handling: per seat, the messages the node's running turn of it handles:
	// the base of the chain of its messages without a reply (seatTurnBase).
	handling map[string][]Message
	// gated: seats whose running turn holds a TurnGate slot; lent: the ones
	// that lent it while they wait for a discuss answer (LendTurn).
	gated, lent map[string]bool
	// doing: what each running node turn does (its agent's stream: setSeatDoing).
	doing map[string]agentDoing
	// ran: the model and reasoning effort each seat's latest turn ran with, as
	// its agent reported them (setSeatRan); stamped on the seat's messages.
	ran map[string]AgentRef
	// dirty: the last save failed; seatsDue saves again.
	dirty bool
	// pauseLogged: seats whose hop-limit pause seatsDue logged (once per pause).
	pauseLogged map[string]bool
}

type seatMark struct {
	at    time.Time
	wake  bool
	turn  bool
	token string
}

func seatKey(seat, id string) string { return seat + "/" + id }

func openSeats(dir string) (*seatStore, error) {
	st := &seatStore{path: filepath.Join(dir, "seats.json"), run: map[string]context.CancelFunc{}, queued: map[string]bool{}, errs: map[string]string{},
		busy: map[string]bool{}, quiet: map[string]bool{}, deferred: map[string]bool{}, marks: map[string]seatMark{}, pauseLogged: map[string]bool{},
		handling: map[string][]Message{}, gated: map[string]bool{}, lent: map[string]bool{}, doing: map[string]agentDoing{}, ran: map[string]AgentRef{}}
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
	out := make([]SeatView, 0, len(st.seats))
	for _, s := range st.seats {
		c := *s
		c.Pending = slices.Clone(s.Pending)
		v := SeatView{Seat: c, Error: st.errs[s.ID], TurnQueued: st.queued[s.ID]}
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
	st.mu.Unlock()
	for i := range out {
		switch {
		case out[i].Status != SeatOffline && out[i].Status != SeatBusy:
		case n.seatAllFailed(out[i].ID, out[i].Pending):
			out[i].Status = SeatNeedsHuman
		case out[i].Status == SeatOffline && n.seatAllPaused(out[i].Pending):
			out[i].Status = SeatPaused
		}
	}
	return out
}

// seatAllFailed reports whether seat has messages to answer and the lease of
// every one failed (a turn that died with the node, or spent its attempts): no
// turn runs for them until a person starts the seat (StartSeat). A message
// past the hop limit waits for a person anyway and does not count.
func (n *Node) seatAllFailed(seat string, pend []SeatPending) bool {
	failed := 0
	for _, p := range pend {
		if l, _ := n.leases.get(leaseKey(seat, p.ID)); l.Failed {
			failed++
			continue
		}
		if rec, ok := n.chats.message(p.ID); ok && !n.seatPaused(p, rec.Message) {
			return false
		}
	}
	return failed > 0
}

// forgetSeatLeases drops the failed lease records of seat's messages ids: a person
// decided they run again (StartSeat, the same message queued again), with no
// attempts or failure.
func (n *Node) forgetSeatLeases(seat string, ids []string) {
	changed := false
	for _, id := range ids {
		k := leaseKey(seat, id)
		if l, ok := n.leases.get(k); !ok || !l.Failed {
			continue // a running turn keeps its lease
		}
		if err := n.leases.forget(k); err != nil {
			n.log.Warn("save leases", "err", err)
		}
		changed = true
	}
	if changed {
		n.changed("leases")
	}
}

// seatAllPaused reports whether pend has messages and every one waits for a
// person (seatPaused): no turn runs for them.
func (n *Node) seatAllPaused(pend []SeatPending) bool {
	if len(pend) == 0 {
		return false
	}
	for _, p := range pend {
		rec, ok := n.chats.message(p.ID)
		if !ok || !n.seatPaused(p, rec.Message) {
			return false
		}
	}
	return true
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

// AddSeat adds a seat of req.Provider and starts its session (StartSeat)
// unless req.Defer.
func (n *Node) AddSeat(req SeatRequest) (SeatView, error) {
	switch {
	case n.cfg.Project == "":
		return SeatView{}, ErrNotProject
	case n.cfg.DisableSeats:
		return SeatView{}, ErrSeatsDisabled
	case n.NeedsFolder():
		return SeatView{}, ErrNeedsFolder
	case req.Provider != ProviderClaude && req.Provider != ProviderCodex:
		return SeatView{}, fmt.Errorf("%w: provider must be %q or %q", ErrBadRequest, ProviderClaude, ProviderCodex)
	}
	st := n.seats
	st.mu.Lock()
	if len(st.seats) >= maxSeats {
		st.mu.Unlock()
		return SeatView{}, fmt.Errorf("%w: %w: at most %d seats", ErrBadRequest, ErrSeatLimit, maxSeats)
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
	if req.Defer {
		st.deferred[s.ID] = true
	}
	err = st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		return SeatView{}, err
	}
	n.log.Info("seat added", "seat", s.ID, "provider", s.Provider, "label", s.Label)
	n.changed("seats")
	if req.Defer {
		return n.seatView(s.ID)
	}
	return n.StartSeat(s.ID, req.Open)
}

// StartSeat lets seat id run again (after StopSeat or a failed turn). A seat
// without a session gets one: a turn with its introduction (and its pending
// messages). open shows the session in the desktop app.
func (n *Node) StartSeat(id string, open bool) (SeatView, error) {
	if n.cfg.DisableSeats {
		return SeatView{}, ErrSeatsDisabled
	}
	st := n.seats
	st.mu.Lock()
	s := st.getLocked(id)
	if s == nil {
		st.mu.Unlock()
		return SeatView{}, fmt.Errorf("%w %s", ErrUnknownSeat, id)
	}
	s.Stopped, s.Fails, s.RetryAt = false, 0, time.Time{}
	delete(st.errs, id)
	delete(st.deferred, id)
	err := st.saveLocked()
	seat, running := *s, st.run[id] != nil
	pend := make([]string, 0, len(s.Pending))
	for _, p := range s.Pending {
		pend = append(pend, p.ID)
	}
	st.mu.Unlock()
	if err != nil {
		return SeatView{}, err
	}
	// The person decides: a message whose lease failed runs again.
	n.forgetSeatLeases(id, pend)
	if n.Stopped() {
		// A seat added or resumed during the global pause keeps its place.
		// seatsDue starts its introduction (or pending work) after resume.
		n.changed("seats")
		return n.seatView(id)
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
// Its messages not answered yet go back to the node's sessions (releaseSeat).
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
	pend := st.seats[i].Pending
	st.seats = slices.Delete(st.seats, i, i+1)
	delete(st.errs, id)
	delete(st.busy, id)
	delete(st.deferred, id)
	delete(st.ran, id)
	delete(st.pauseLogged, id)
	for k := range st.marks {
		if strings.HasPrefix(k, id+"/") {
			delete(st.marks, k)
		}
	}
	err := st.saveLocked()
	st.mu.Unlock()
	n.releaseSeat(id, pend)
	n.changed("seats")
	return err
}

// releaseSeat hands the messages pend of removed seat back to normal routing:
// a peer's message assigned to it is unread again for the node's sessions,
// and its leases of the seat go (a running turn ends with no record).
func (n *Node) releaseSeat(seat string, pend []SeatPending) {
	back := false
	for _, p := range pend {
		ok, err := n.chats.unassign(p.ID, "seat:"+seat)
		if err != nil {
			n.log.Warn("return a removed seat's message", "seat", seat, "id", p.ID, "err", err)
		}
		back = back || ok
		if err := n.leases.forget(leaseKey(seat, p.ID)); err != nil {
			n.log.Warn("save leases", "err", err)
		}
	}
	if len(pend) > 0 {
		n.log.Info("a removed seat's messages went back to the sessions", "seat", seat, "messages", len(pend))
		n.changed("leases")
	}
	if back {
		n.changed("messages")
	}
}

// dropSeats removes every seat of a project without seats (DisableSeats):
// seats left from before go, their messages back to the sessions.
func (n *Node) dropSeats() {
	st := n.seats
	st.mu.Lock()
	var ids []string
	for _, s := range st.seats {
		ids = append(ids, s.ID)
	}
	st.mu.Unlock()
	for _, id := range ids {
		if err := n.RemoveSeat(id); err != nil {
			n.log.Warn("remove a seat of a project without seats", "seat", id, "err", err)
		}
	}
}

// WaitSeatTurns waits up to d until no turn of a seat runs (RemoveSeat and
// StopSeat cancel them; a turn ends once its agent process exits). It reports
// false when one still runs.
func (n *Node) WaitSeatTurns(d time.Duration) bool {
	st := n.seats
	for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
		st.mu.Lock()
		running := len(st.run)
		st.mu.Unlock()
		if running == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
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
		ran := st.ran[s.ID]
		ref = &AgentRef{Seat: s.ID, Label: s.Label, Provider: s.Provider, Model: ran.Model, Effort: ran.Effort}
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

// DiscussSeat picks the seat of provider that req, a message without a reply
// to be sent in chat (discuss), asks: never the sender's own seat (the node
// drops it from AskSeats, resolveSeats), and within the hop limit never one
// whose running turn is in the same automatic chain (inheritChain): that turn
// waits upstream for this very answer, so asking it would wait until the
// timeout. "" when none fits: the caller adds a seat. Past the hop limit the
// message waits for a person (no seat answers it automatically), so any other
// seat of provider may keep it.
func (n *Node) DiscussSeat(provider, chat string, req SendRequest) (string, error) {
	agent, err := n.senderAgent(req)
	if err != nil {
		return "", err
	}
	m := Message{AuthorKind: req.AuthorKind, Agent: agent}
	if c, ok := n.chats.get(chat); ok {
		n.inheritChain(c, &m)
	}
	held := n.overDepth(m)
	sender := ""
	if agent != nil {
		sender = agent.Seat
	}
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, s := range st.seats {
		if s.Provider != provider || s.ID == sender {
			continue
		}
		if !held && m.RootID != "" && st.run[s.ID] != nil && slices.ContainsFunc(st.handling[s.ID], func(h Message) bool {
			return cmp.Or(h.RootID, h.ID) == m.RootID
		}) {
			continue
		}
		return s.ID, nil
	}
	return "", nil
}

// SeatAskState is why seat does not answer pending message id by itself:
// Error is its last failed turn's, RetryAt when it tries again; Stuck when it
// does not try again before deadline (a person decides: its tries are spent,
// the message's lease failed, or the retry comes later).
type SeatAskState struct {
	Error   string
	RetryAt time.Time
	Stuck   bool
}

// SeatAsk reports the state of seat's pending message id (SeatAskState); the
// zero state while the seat may still answer it before deadline.
func (n *Node) SeatAsk(seat, id string, deadline time.Time) SeatAskState {
	failed := false
	if l, ok := n.leases.get(leaseKey(seat, id)); ok {
		failed = l.Failed
	}
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.getLocked(seat)
	if s == nil || st.run[s.ID] != nil || s.Fails == 0 && !failed {
		return SeatAskState{}
	}
	out := SeatAskState{Error: st.errs[s.ID]}
	if !s.needsHuman() {
		out.RetryAt = s.RetryAt
	}
	// A turn that gave no reply (ErrNoReply) is told at once: its retry may
	// give none either.
	out.Stuck = failed || s.needsHuman() || s.RetryAt.After(deadline) || usageLimited(out.Error) ||
		strings.Contains(out.Error, ErrNoReply.Error())
	return out
}

// usageLimited reports a provider's usage or quota limit in a turn's error
// ("You've hit your usage limit … try again at …"): its retries within minutes
// fail the same way.
func usageLimited(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "usage limit") || strings.Contains(msg, "quota")
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

// unqueueSeat takes message id back from the pending messages of seat ("":
// none): seatsForIncoming queued it but storing it failed.
func (n *Node) unqueueSeat(seat, id string) {
	if seat == "" {
		return
	}
	st := n.seats
	st.mu.Lock()
	var err error
	if s := st.getLocked(seat); s != nil {
		s.Pending = slices.DeleteFunc(s.Pending, func(p SeatPending) bool { return p.ID == id })
		err = st.saveLocked()
	}
	st.mu.Unlock()
	if err != nil {
		n.log.Warn("save seats; saved again later", "err", err)
	}
	n.changed("seats")
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
	if len(list) == 0 || n.cfg.DisableSeats {
		return false, nil
	}
	now := time.Now().UTC()
	st := n.seats
	var again []string // seats that get id once more: its failure is forgotten
	st.mu.Lock()
	for _, t := range list {
		s := st.getLocked(t.seat)
		if s == nil {
			continue
		}
		queued = true
		if slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == id }) {
			again = append(again, s.ID)
			continue
		}
		s.Pending = append(s.Pending, SeatPending{ID: id, Ask: t.ask, At: now})
		s.Fails, s.RetryAt = 0, time.Time{} // a new message: a failed seat runs again
	}
	if queued {
		err = st.saveLocked()
	}
	st.mu.Unlock()
	for _, s := range again {
		n.forgetSeatLeases(s, []string{id})
	}
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
	if n.cfg.DisableSeats {
		return nil, nil
	}
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
	if n.cfg.DisableSeats {
		return false, false
	}
	st := n.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.bySessionLocked(session)
	if s == nil || s.Stopped || !slices.ContainsFunc(s.Pending, func(p SeatPending) bool { return p.ID == id }) {
		return false, false
	}
	if st.quiet[s.ID] {
		// Its setup turn posts nothing (senderAgent): the message waits for
		// the seat's next turn instead of a hook delivering it now.
		return true, false
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
	if n.cfg.DisableSeats {
		return ""
	}
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
	if n.cfg.DisableSeats {
		return
	}
	dl, ok := n.launcher.(DirectLauncher)
	if !ok {
		return
	}
	n.dropOrphanPending(now)
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
		if s.Stopped || st.deferred[s.ID] || st.run[s.ID] != nil || (len(s.Pending) == 0 && s.SessionID != "") || (s.SessionID != "" && live[s.SessionID]) ||
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
		ready := s.SessionID == "" && len(s.Pending) == 0
		paused := 0
		for _, p := range s.Pending {
			if l, _ := n.leases.get(leaseKey(s.ID, p.ID)); l.Failed {
				continue // a person decides
			}
			rec, ok := n.chats.message(p.ID)
			if !ok {
				continue
			}
			if n.seatPaused(p, rec.Message) {
				paused++
				continue
			}
			ready = true
			break
		}
		n.notePaused(s.ID, !ready && paused > 0, paused)
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

// orphanGrace is how long a seat's pending message may lack its stored record
// before dropOrphanPending drops it: seatsForIncoming queues a message just
// before storing it.
const orphanGrace = time.Minute

// dropOrphanPending drops the pending messages of seats whose record is gone
// (never stored, or its chat removed) past orphanGrace: no turn, hook or wake
// could ever deliver them, and they would keep the seat waiting.
func (n *Node) dropOrphanPending(now time.Time) {
	st := n.seats
	st.mu.Lock()
	var old []string
	for _, s := range st.seats {
		for _, p := range s.Pending {
			if now.Sub(p.At) > orphanGrace {
				old = append(old, p.ID)
			}
		}
	}
	st.mu.Unlock()
	gone := map[string]bool{}
	for _, id := range old {
		if _, ok := n.chats.message(id); !ok {
			gone[id] = true
		}
	}
	if len(gone) == 0 {
		return
	}
	st.mu.Lock()
	for _, s := range st.seats {
		s.Pending = slices.DeleteFunc(s.Pending, func(p SeatPending) bool {
			if gone[p.ID] {
				n.log.Warn("seat message without its record dropped", "seat", s.ID, "id", p.ID)
				delete(st.marks, seatKey(s.ID, p.ID))
				return true
			}
			return false
		})
	}
	err := st.saveLocked()
	st.mu.Unlock()
	if err != nil {
		n.log.Warn("save seats; saved again later", "err", err)
	}
	n.changed("seats")
}

// notePaused logs once, when seat's messages start waiting for a person past
// the hop limit (paused of them), and forgets it when they stop waiting.
func (n *Node) notePaused(seat string, on bool, paused int) {
	st := n.seats
	st.mu.Lock()
	was := st.pauseLogged[seat]
	if on {
		st.pauseLogged[seat] = true
	} else {
		delete(st.pauseLogged, seat)
	}
	st.mu.Unlock()
	if on && !was {
		n.log.Info("seat paused: its messages are past the hop limit and wait for a person", "seat", seat,
			"paused", paused, "max_auto_depth", n.maxDepth())
		n.changed("seats")
	}
}

// startSeatTurn runs one turn of seat id in the background: its introduction
// first when intro, then its pending messages (as many as fit), resuming its
// session (a new one when it has none). It reports false when the node does
// not run or has no DirectLauncher.
func (n *Node) startSeatTurn(id string, intro, open bool) bool {
	if n.cfg.DisableSeats {
		return false
	}
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
	st.mu.Unlock()
	n.changed("seats")
	defer func() {
		st.mu.Lock()
		delete(st.run, id)
		delete(st.queued, id)
		delete(st.quiet, id)
		delete(st.handling, id)
		delete(st.doing, id)
		st.mu.Unlock()
		n.changed("seats")
	}()
	// Past the gate's cap the turn waits for a slot, marked running (no
	// second turn of the seat starts) and queued; Stop or Remove ends the wait.
	if g := n.turnGate; g != nil {
		if !g.tryAcquire() {
			st.mu.Lock()
			st.queued[id] = true
			st.mu.Unlock()
			n.changed("seats")
			if !g.acquire(ctx) {
				return
			}
			st.mu.Lock()
			delete(st.queued, id)
			st.mu.Unlock()
		}
		st.mu.Lock()
		st.gated[id] = true
		st.mu.Unlock()
		defer func() {
			// A slot lent while the turn waits for a discuss answer is not the
			// turn's any more (LendTurn).
			st.mu.Lock()
			lent := st.lent[id]
			delete(st.lent, id)
			delete(st.gated, id)
			st.mu.Unlock()
			if !lent {
				g.release()
			}
		}()
	}
	st.mu.Lock()
	s = st.getLocked(id)
	if s == nil || s.Stopped {
		st.mu.Unlock()
		return
	}
	seat := *s
	seat.Pending = slices.Clone(s.Pending)
	st.mu.Unlock()
	n.runSeatTurn(ctx, dl, seat, intro, open)
}

// MaxParallelTurns is the default cap of TurnGate: the node turns of seats
// (headless agent processes) that run at once on this machine.
const MaxParallelTurns = 4

// TurnGate caps the seat turns that run at once across the nodes sharing it
// (SetTurnGate); a turn past the cap waits for a slot. A turn waiting for a
// discuss answer lends its slot (LendTurn), so a chain of seats asking each
// other never waits on its own slots; the slot it takes back when the cap is
// full is a debt the next release pays instead of freeing a slot.
type TurnGate struct {
	slots chan struct{}
	mu    sync.Mutex
	debt  int
}

// NewTurnGate is a gate of max slots (at least one).
func NewTurnGate(maxTurns int) *TurnGate {
	return &TurnGate{slots: make(chan struct{}, max(maxTurns, 1))}
}

func (g *TurnGate) tryAcquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// acquire waits for a slot; false when ctx ended first.
func (g *TurnGate) acquire(ctx context.Context) bool {
	select {
	case g.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// release frees the slot of a turn (or pays a debt): the slots in the gate
// plus the debt count the turns holding one, so a release never blocks.
func (g *TurnGate) release() {
	g.mu.Lock()
	if g.debt > 0 {
		g.debt--
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	<-g.slots
}

// lend frees the slot of a turn that keeps running (restore takes it back).
func (g *TurnGate) lend() { g.release() }

// restore takes back a lent slot: a free one, else a debt (the cap is
// exceeded until the next release).
func (g *TurnGate) restore() {
	select {
	case g.slots <- struct{}{}:
	default:
		g.mu.Lock()
		g.debt++
		g.mu.Unlock()
	}
}

// LendTurn lends the TurnGate slot of seat's running node turn while the turn
// waits for a discuss answer: the seats it asks (and any they ask in turn)
// run even when the chain's waiting turns hold every slot. restore takes the
// slot back; it does nothing when nothing was lent or the turn ended since.
func (n *Node) LendTurn(seat string) (restore func()) {
	g := n.turnGate
	st := n.seats
	if g == nil || seat == "" {
		return func() {}
	}
	st.mu.Lock()
	ok := st.gated[seat] && !st.lent[seat]
	if ok {
		st.lent[seat] = true
		g.lend()
	}
	st.mu.Unlock()
	if !ok {
		return func() {}
	}
	return func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.lent[seat] {
			delete(st.lent, seat)
			g.restore()
		}
	}
}

// SetTurnGate shares gate g's cap with this node's seat turns (nil: no cap).
// It must be set before Run.
func (n *Node) SetTurnGate(g *TurnGate) { n.turnGate = g }

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
	n.seats.setHandling(seat.ID, msgs)
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
		prompt.WriteString(wakePrompt(msgs, ready-len(msgs), folder, randomHex(8), true))
	}
	// The turn's final answer (reported: its launcher reads one) is the reply
	// to the messages that ask the seat (answerSeatAsks).
	var answer string
	reported := false
	spec := LaunchSpec{Provider: seat.Provider, Folder: folder, ResumeID: seat.SessionID, Prompt: prompt.String(),
		Seat: seat.ID, NoOpen: !open, Env: n.seatEnv(seat, chat),
		Doing:  func(kind string, subs int) { n.setSeatDoing(seat.ID, kind, subs) },
		Ran:    func(model, effort string) { n.setSeatRan(seat.ID, model, effort) },
		Answer: func(text string) { answer, reported = text, true }}
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
	answered := msgs
	var missing []string
	if err == nil && reported {
		// A message that asked the seat is done only with a reply to it: the
		// seat's own (agentlink send) or its final answer, posted here.
		missing = n.answerSeatAsks(seat.ID, msgs, answer)
		answered = slices.DeleteFunc(slices.Clone(msgs), func(m UnreadMessage) bool { return slices.Contains(missing, m.ID) })
		if len(missing) > 0 {
			err = fmt.Errorf("%w (%d of its messages)", ErrNoReply, len(missing))
			n.log.Warn("a seat's turn ended without a reply", "seat", seat.ID, "messages", missing)
		}
	}
	if (err == nil || len(missing) > 0) && len(answered) > 0 {
		// Acknowledged under the turn's claim: no hook takes them in between.
		done := n.seatAck("", seat.ID, ids(answered))
		if err := n.leases.ack(seat.ID, nil, done, time.Now()); err != nil {
			n.log.Warn("save leases", "err", err)
		}
		n.changed("messages")
	}
	switch {
	case len(missing) > 0:
		n.revokeLeases(owner, missing, "seat_no_reply", false)
	case err != nil && len(msgs) > 0:
		n.revokeLeases(owner, ids(msgs), "seat_turn_failed", false)
	}
	n.endTurn(seat.ID, msgs, err, stopped)
}

// ErrNoReply: a seat's turn succeeded but gave no reply to a message that
// asked it (no agentlink send reply, no final answer): the message stays
// pending, the turn counts as failed (seatRetry) and a discuss waiting for it
// learns why (SeatAsk).
var ErrNoReply = errors.New("the agent's turn ended without a reply")

// answerSeatAsks posts answer, the final answer of seat's turn, as the seat's
// reply to each of msgs that asked it and has no reply of the seat yet (one it
// sent itself in the turn is kept: no second copy). It returns the ids of the
// asks left without a reply (no answer, or the post failed).
func (n *Node) answerSeatAsks(seat string, msgs []UnreadMessage, answer string) (missing []string) {
	answer = strings.TrimSpace(answer)
	for _, m := range msgs {
		if !m.AsksYou || m.Kind != "" || n.seatReplied(m.ChatID, seat, m.ID) {
			continue
		}
		if answer == "" {
			missing = append(missing, m.ID)
			continue
		}
		sent, err := n.SendRequest(SendRequest{ChatID: m.ChatID, ReplyTo: m.ID, Body: answer, AuthorKind: AuthorAgent, Seat: seat})
		if sent.ID == "" {
			n.log.Warn("post a seat's answer", "seat", seat, "id", m.ID, "err", err)
			missing = append(missing, m.ID)
			continue
		}
		if err != nil {
			n.log.Warn("post a seat's answer", "seat", seat, "id", m.ID, "reply", sent.ID, "err", err)
		}
		n.log.Info("posted a seat's final answer as its reply", "seat", seat, "id", m.ID, "reply", sent.ID)
	}
	return missing
}

// seatReplied reports whether seat of this node replied to message id in chat
// (the reply a discuss waits for).
func (n *Node) seatReplied(chat, seat, id string) bool {
	s, ok := n.chats.snapshot(chat)
	if !ok {
		return false
	}
	for _, rec := range slices.Backward(s.msgs) {
		r := rec.Message
		if r.ID == id {
			return false
		}
		if r.Kind == "" && r.ReplyTo == id && r.From == n.cfg.Node && r.Agent != nil && r.Agent.Seat == seat {
			return true
		}
	}
	return false
}

// setHandling records msgs as the messages the node's running turn of seat
// handles (seatTurn forgets them when it ends).
func (st *seatStore) setHandling(seat string, msgs []UnreadMessage) {
	list := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Kind == "" {
			list = append(list, m.Message)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(list) == 0 {
		delete(st.handling, seat)
		return
	}
	st.handling[seat] = list
}

// seatTurnBase is the part of the active chain base (activeChainBase) of
// seat's message in chat c that the node's running turn of it gives, nil
// outside a turn or for a turn of its introduction alone:
//   - reset: the newest person's prompt the turn handles in c while the seat
//     has not answered it yet (answeredSince): a new prompt resets its own
//     answer, and only its first one;
//   - else the deepest agent message the turn handles in c (in any chat when
//     none is in c, so switching chats escapes nothing; a prompt of another
//     chat never resets c's chain). The caller takes the deeper of it and
//     what reached the seat since it last wrote, so hops within one long
//     turn keep counting.
func (n *Node) seatTurnBase(seat string, c Chat) (base *Message, reset bool) {
	st := n.seats
	st.mu.Lock()
	handled := slices.Clone(st.handling[seat])
	st.mu.Unlock()
	inChat := slices.DeleteFunc(slices.Clone(handled), func(m Message) bool { return m.ChatID != c.ID })
	var human *Message
	for _, m := range inChat {
		if m.AuthorKind == AuthorHuman && (human == nil || m.CreatedAt.After(human.CreatedAt)) {
			human = &m
		}
	}
	if human != nil && !n.answeredSince(c, seat, human.ID) {
		return human, true
	}
	if len(inChat) > 0 {
		handled = inChat
	}
	for _, m := range handled {
		if m.AuthorKind != AuthorHuman {
			base = deeper(base, m)
		}
	}
	return base, false
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
	if exe := SelfExe(); exe != "" {
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
		"Спросить другого агента: agentlink send --chat %s --ask-seat <имя> --body-file <файл с текстом> "+
		"(список агентов: agentlink seats) "+ReplyTextNote+". "+
		"Ответ на сообщение, которое просит ответа от вас, — ваше итоговое сообщение хода: agent-link сам отправит его ответом, agentlink send для этого не нужен; "+
		"только когда у сообщения указана команда ответа (agentlink send --chat %s --reply-to <id> --body-file <файл>), ответьте ею. "+
		"Никогда не пишите в чат по своей инициативе (ни приветствий, ни представлений): только ответ на сообщение, "+
		"которое просит ответа от вас, или вопрос, без которого эту работу не сделать. Ответ на ваш вопрос придёт сам.",
		seat.Label, ProviderName(seat.Provider), name, n.cfg.Node, chat, with, chat, chat)
	if setup {
		intro += " Сейчас ничего не отправляйте и ничего не делайте, просто завершите ход: сообщения agent-link для вас придут в следующем ходе."
	}
	return intro
}
