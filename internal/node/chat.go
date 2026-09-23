package node

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/config"
)

// CapChat: takes part in chats (Message.ChatID and the chat_open/chat_close
// kinds). Chat messages go only to peers that announce it.
const CapChat = "chat-v1"

// Chat errors.
var (
	// ErrUnknownChat: no chat with that id is known here.
	ErrUnknownChat = errors.New("unknown chat")
	// ErrChatClosed: the chat is closed; continuing the topic takes a new chat.
	ErrChatClosed = errors.New("chat is closed")
	// ErrNoChatSupport: a participant is not connected with chat support now.
	ErrNoChatSupport = errors.New("participant is not connected with chat support")
	// ErrBadParticipants: a chat needs at least one other known member, and
	// only participants other than the author can be asked.
	ErrBadParticipants = errors.New("invalid chat participants")
	// ErrLegacyChat: a virtual chat built from pre-chat history cannot take
	// messages of its own (sending to it continues it in a real chat).
	ErrLegacyChat = errors.New("history from before chats")
)

// Chat is one conversation with a fixed set of participants (sorted, this node
// included). Changing who takes part takes a new chat. Area is fixed at
// creation and picks the project the participants' agents work in. A chat is
// open until a participant closes it; a closed chat never opens again.
type Chat struct {
	ID           string    `json:"id"`
	Participants []string  `json:"participants"`
	Area         string    `json:"area,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	CloseID      string    `json:"close_id,omitempty"`
	ClosedBy     string    `json:"closed_by,omitempty"`
	ClosedAt     time.Time `json:"closed_at,omitzero"`
}

// Closed reports whether a participant closed the chat.
func (c Chat) Closed() bool { return c.CloseID != "" }

// ChatView is this node's own view of a legacy chat (local history): archived
// when this node closed it. A real chat is archived exactly when it is closed.
type ChatView struct {
	Archived bool      `json:"archived"`
	At       time.Time `json:"at,omitzero"`
}

// Delivery is where one copy of an outbound chat message is: "queued" until
// the peer ACKs it, then "sent".
type Delivery struct {
	Peer   string `json:"peer"`
	Status string `json:"status"`
}

// ChatMessage is one message of a chat as listed by the chat API: one entry
// per message id, however many peers it went to. Seq is this node's order.
type ChatMessage struct {
	Seq       uint64     `json:"seq"`
	Direction string     `json:"direction"` // "in" or "out"
	Held      bool       `json:"held,omitempty"`
	Delivery  []Delivery `json:"delivery,omitempty"` // out only
	Message
}

// JobActivity is a job a participant runs or queues for a chat request.
type JobActivity struct {
	ReplyTo      string         `json:"reply_to"`
	JobStatus    string         `json:"job_status"`
	Activity     string         `json:"activity,omitempty"`
	ActivityInfo *ActivityState `json:"activity_info,omitempty"`
	UpdatedAt    time.Time      `json:"updated_at"`
	// Stale: the participant is disconnected, so this may be over.
	Stale bool `json:"stale,omitempty"`
}

// ParticipantState is one participant of a chat as this node sees it.
type ParticipantState struct {
	Name      string `json:"name"`
	Self      bool   `json:"self,omitempty"`
	Connected bool   `json:"connected"`
	// Compatible is false while the participant is connected without chat
	// support: chat delivery to it waits until it upgrades.
	Compatible bool          `json:"compatible"`
	Queued     int           `json:"queued"` // messages of the chat not yet ACKed by it
	Jobs       []JobActivity `json:"jobs,omitempty"`
}

// ChatInfo describes a chat for the chat list and the chat page.
type ChatInfo struct {
	Chat
	Closed   bool `json:"closed"`
	Archived bool `json:"archived"`
	// Legacy marks a virtual chat built from history before chats: one
	// request with its replies, with one peer. Sending to it continues it in a
	// real chat (continueLegacy); closing it only archives it on this node.
	Legacy bool   `json:"legacy,omitempty"`
	Peer   string `json:"peer,omitempty"` // legacy only: the other side
	Title  string `json:"title"`          // the first message, shortened
	// Count and LastMessage cover messages (kind ""), not control messages.
	Count       int          `json:"count"`
	LastSeq     uint64       `json:"last_seq"`
	LastMessage *ChatMessage `json:"last_message,omitempty"`
	LastAt      time.Time    `json:"last_at"`
	// Active: some participant runs or queues a job for the chat.
	Active  bool               `json:"active"`
	Members []ParticipantState `json:"members"`
}

// legacyPrefix starts the id of a virtual chat: legacy-<root id>-<peer>.
const legacyPrefix = "legacy-"

func legacyChatID(root, peer string) string { return legacyPrefix + root + "-" + peer }

func parseLegacyChatID(id string) (root, peer string, ok bool) {
	rest, ok := strings.CutPrefix(id, legacyPrefix)
	if !ok || len(rest) < 34 || rest[32] != '-' || !validID(rest[:32]) || !config.ValidName(rest[33:]) {
		return "", "", false
	}
	return rest[:32], rest[33:], true
}

// normalizeParticipants sorts and dedupes names and adds this node.
func (n *Node) normalizeParticipants(names []string) ([]string, error) {
	out := []string{n.cfg.Node}
	for _, s := range names {
		for p := range strings.SplitSeq(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) < 2 {
		return nil, fmt.Errorf("%w: name at least one other member", ErrBadParticipants)
	}
	return out, nil
}

// CreateChat starts a chat of this node and the members in with (names, also
// comma-separated). Every other participant must be connected and announce
// chat support now; the chat is then announced to all of them.
func (n *Node) CreateChat(with []string, area string) (ChatInfo, error) {
	parts, err := n.normalizeParticipants(with)
	if err != nil {
		return ChatInfo{}, err
	}
	if area != "" && !config.ValidName(area) {
		return ChatInfo{}, fmt.Errorf("invalid area %q", area)
	}
	var missing []string
	for _, p := range parts {
		if p == n.cfg.Node {
			continue
		}
		n.mu.Lock()
		ok := n.known[p] && !n.removedLocked(p)
		n.mu.Unlock()
		if !ok {
			return ChatInfo{}, fmt.Errorf("%w %q", ErrUnknownPeer, p)
		}
		if !n.PeerHas(p, CapChat) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return ChatInfo{}, fmt.Errorf("%w: %s", ErrNoChatSupport, strings.Join(missing, ", "))
	}
	c := Chat{ID: newID(), Participants: parts, Area: area, CreatedAt: time.Now().UTC()}
	if _, err := n.chats.ensure(c); err != nil {
		return ChatInfo{}, err
	}
	open := Message{ID: DerivedID(c.ID, "open"), Kind: KindChatOpen, CreatedAt: c.CreatedAt}
	if err := n.postChat(c, open); err != nil {
		return ChatInfo{}, err
	}
	n.changed("chats")
	return n.Chat(c.ID)
}

// CloseChat closes a chat for every participant, which moves it to the
// archive on every node: a chat is archived exactly when it is closed, and
// only people close chats. Closing a closed chat is a no-op. Jobs already
// running finish; their replies are still kept. A legacy chat (local history
// only) is moved to this node's archive.
func (n *Node) CloseChat(id string) (ChatInfo, error) {
	if _, _, ok := parseLegacyChatID(id); ok {
		if _, err := n.Chat(id); err != nil {
			return ChatInfo{}, err
		}
		if err := n.chats.setView(id, ChatView{Archived: true, At: time.Now().UTC()}); err != nil {
			return ChatInfo{}, err
		}
		n.changed("chats")
		return n.Chat(id)
	}
	c, ok := n.chats.get(id)
	if !ok {
		return ChatInfo{}, fmt.Errorf("%w %s", ErrUnknownChat, id)
	}
	if !c.Closed() {
		m := Message{ID: DerivedID(c.ID, "close/"+n.cfg.Node), Kind: KindChatClose, CreatedAt: time.Now().UTC()}
		if err := n.postChat(c, m); err != nil {
			return ChatInfo{}, err
		}
		n.changed("chats")
	}
	return n.Chat(id)
}

// ChatSend is a message for a chat from the control API.
type ChatSend struct {
	ChatID  string
	Body    string
	ReplyTo string
	Ask     []string // participants asked to answer (names, also comma-separated)
	// Parent is the request whose job sends this (AGENTLINK_JOB_ID): the new
	// message continues its automatic chain (RootID, AutoDepth + 1).
	Parent string
}

// SendChat posts a message to a chat. Without Ask it only informs. A legacy
// chat is continued (continueLegacy).
func (n *Node) SendChat(s ChatSend) (Message, error) {
	if _, peer, ok := parseLegacyChatID(s.ChatID); ok {
		return n.continueLegacy(peer, s)
	}
	m := Message{ChatID: s.ChatID, Body: s.Body, ReplyTo: s.ReplyTo}
	for _, a := range s.Ask {
		for p := range strings.SplitSeq(a, ",") {
			if p = strings.TrimSpace(p); p != "" {
				m.Responders = append(m.Responders, p)
			}
		}
	}
	if s.Parent != "" {
		if p, ok := n.chats.message(s.Parent); ok && p.Message.Kind == "" {
			m.RootID = cmp.Or(p.Message.RootID, p.Message.ID)
			m.AutoDepth = p.Message.AutoDepth
			if m.AutoDepth < 255 {
				m.AutoDepth++
			}
		}
	}
	return n.SendMessage(m)
}

// continueLegacy sends s, addressed to legacy chat s.ChatID with peer, as the
// next message of that conversation: into the newest open chat of just this
// node and peer with the same area, a new one when there is none. A peer
// connected without chat support gets a plain message instead. The reply
// reference is kept only for the plain message: a real chat does not hold the
// legacy messages.
func (n *Node) continueLegacy(peer string, s ChatSend) (Message, error) {
	legacy, err := n.Chat(s.ChatID)
	if err != nil {
		return Message{}, err
	}
	if n.Connected(peer) && !n.PeerHas(peer, CapChat) {
		return n.Send(peer, s.Body, s.ReplyTo)
	}
	open, err := n.openChatWith(peer, legacy.Area)
	if err != nil {
		return Message{}, err
	}
	s.ChatID, s.ReplyTo = open.ID, ""
	return n.SendChat(s)
}

// openChatWith returns the newest open chat of just this node and peer with
// area, creating one when there is none.
func (n *Node) openChatWith(peer, area string) (Chat, error) {
	parts := []string{n.cfg.Node, peer}
	slices.Sort(parts)
	var open *Chat
	for _, cs := range n.chats.all() {
		c := cs.chat
		if !c.Closed() && c.Area == area && slices.Equal(c.Participants, parts) &&
			(open == nil || c.CreatedAt.After(open.CreatedAt)) {
			open = &c
		}
	}
	if open != nil {
		return *open, nil
	}
	info, err := n.CreateChat([]string{peer}, area)
	return info.Chat, err
}

// sendChat is SendMessage for a chat message or status update.
func (n *Node) sendChat(m Message) (Message, error) {
	if _, _, ok := parseLegacyChatID(m.ChatID); ok {
		return Message{}, ErrLegacyChat
	}
	c, ok := n.chats.get(m.ChatID)
	if !ok {
		return Message{}, fmt.Errorf("%w %s", ErrUnknownChat, m.ChatID)
	}
	if c.Closed() && m.Kind == "" && m.JobStatus == "" {
		return Message{}, ErrChatClosed
	}
	if m.Kind == KindStatus {
		m.Responders = nil
	}
	slices.Sort(m.Responders)
	m.Responders = slices.Compact(m.Responders)
	for _, r := range m.Responders {
		if r == n.cfg.Node || !slices.Contains(c.Participants, r) {
			return Message{}, fmt.Errorf("%w: %q is not another participant", ErrBadParticipants, r)
		}
	}
	if m.Kind == "" {
		if old, ok := n.chats.message(m.ID); ok && old.Message.ChatID == c.ID {
			m = old.Message // a retry keeps its time
		} else {
			m.CreatedAt = time.Now().UTC()
		}
		if m.RootID == "" {
			m.RootID, m.AutoDepth = m.ID, 0
		}
	} else {
		m.CreatedAt = time.Now().UTC()
	}
	if err := n.postChat(c, m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// postChat stores m in chat c, then queues a copy for every other participant.
// A status update is only kept as its job's latest status.
func (n *Node) postChat(c Chat, m Message) error {
	m.From, m.To, m.ChatID, m.Participants, m.Area = n.cfg.Node, "", c.ID, c.Participants, c.Area
	if m.Kind == KindStatus {
		n.chats.noteStatus(m)
	} else if _, _, _, err := n.chats.add(m); err != nil {
		return err
	}
	return n.fanout(c, m, false)
}

// fanout queues m for every participant but this node. With missingOnly only
// peers that have no copy queued or ACKed get one (recovery after a crash).
func (n *Node) fanout(c Chat, m Message, missingOnly bool) error {
	for _, p := range c.Participants {
		if p == n.cfg.Node || (missingOnly && n.store.delivery(p, m.ID) != "") {
			continue
		}
		cp := m
		cp.To = p
		if err := n.enqueue(p, cp); err != nil {
			return err
		}
	}
	return nil
}

// repairChats queues what a crash between storing and fanning out a chat
// message left undelivered.
func (n *Node) repairChats() error {
	for _, s := range n.chats.all() {
		for _, r := range s.msgs {
			if r.Message.From != n.cfg.Node {
				continue
			}
			for _, p := range s.chat.Participants {
				if p == n.cfg.Node || n.store.delivery(p, r.Message.ID) != "" {
					continue
				}
				cp := r.Message
				cp.To = p
				if err := n.store.enqueue(p, cp); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validChatEnvelope checks a chat message from peer: a known kind, the full
// participant list with the author and this node, and responders among them.
func (n *Node) validChatEnvelope(peer string, m *Message) bool {
	switch m.Kind {
	case "", KindStatus, KindChatOpen, KindChatClose:
	default:
		return false
	}
	p := m.Participants
	if !validID(m.ChatID) || len(p) < 2 || !slices.IsSorted(p) || len(slices.Compact(slices.Clone(p))) != len(p) ||
		!slices.Contains(p, peer) || !slices.Contains(p, n.cfg.Node) || (m.Area != "" && !config.ValidName(m.Area)) {
		return false
	}
	for _, r := range m.Responders {
		if r == peer || !slices.Contains(p, r) {
			return false
		}
	}
	for _, name := range p {
		if !config.ValidName(name) {
			return false
		}
	}
	return true
}

// receiveChat stores an inbound chat message. It reports false when the
// message is invalid and must be dropped.
func (n *Node) receiveChat(peer string, m Message) bool {
	if !n.validChatEnvelope(peer, &m) {
		return false
	}
	created, err := n.chats.ensure(Chat{ID: m.ChatID, Participants: m.Participants, Area: m.Area, CreatedAt: m.CreatedAt})
	if err != nil {
		n.log.Warn("chat message rejected", "peer", peer, "chat", m.ChatID, "err", err)
		return false
	}
	closed := false
	if m.Kind == KindStatus {
		if n.chats.noteStatus(m) {
			n.changed("messages")
		}
	} else {
		_, isNew, c, err := n.chats.add(m)
		if err != nil {
			n.log.Error("persist chat message", "chat", m.ChatID, "id", m.ID, "err", err)
			return false
		}
		closed = c
		if isNew {
			n.changed("messages")
		}
	}
	if created || closed {
		n.changed("chats")
	}
	return true
}

// ClaimRun reports whether this node's handler should answer m and records
// the run: m must ask this node, the chat be open, m within MaxAutoDepth, and
// no other request of m's automatic chain (RootID) have run here. Asking the
// same m again gives the same answer, so a resent duplicate is safe.
func (n *Node) ClaimRun(m Message) (bool, error) {
	if !m.Asks(n.cfg.Node) || m.Held() {
		return false, nil
	}
	c, ok := n.chats.get(m.ChatID)
	if !ok || c.Closed() {
		return false, nil
	}
	return n.chats.claimRun(c.ID, cmp.Or(m.RootID, m.ID), m.ID)
}

// ChatOf returns a chat's description; ok is false for an unknown chat.
func (n *Node) ChatOf(id string) (Chat, bool) { return n.chats.get(id) }

// Chat describes one chat, a legacy one too.
func (n *Node) Chat(id string) (ChatInfo, error) {
	if root, peer, ok := parseLegacyChatID(id); ok {
		chats, err := n.legacyChats()
		if err != nil {
			return ChatInfo{}, err
		}
		for _, lc := range chats {
			if lc.root == root && lc.peer == peer {
				return lc.info, nil
			}
		}
		return ChatInfo{}, fmt.Errorf("%w %s", ErrUnknownChat, id)
	}
	s, ok := n.chats.snapshot(id)
	if !ok {
		return ChatInfo{}, fmt.Errorf("%w %s", ErrUnknownChat, id)
	}
	return n.chatInfo(s, n.queuedByChat()), nil
}

// Chats lists chats, busy ones first, then by their last message. archived
// picks the archive instead of the main list; legacy adds the virtual chats of
// history from before chats.
func (n *Node) Chats(archived, legacy bool) ([]ChatInfo, error) {
	queued := n.queuedByChat()
	out := []ChatInfo{}
	for _, s := range n.chats.all() {
		if info := n.chatInfo(s, queued); info.Archived == archived {
			out = append(out, info)
		}
	}
	if legacy {
		chats, err := n.legacyChats()
		if err != nil {
			return nil, err
		}
		for _, lc := range chats {
			if lc.info.Archived == archived {
				out = append(out, lc.info)
			}
		}
	}
	slices.SortFunc(out, func(a, b ChatInfo) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		if c := b.LastAt.Compare(a.LastAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// ChatMessages lists a chat's messages in Seq order (control messages
// included): with after, the first limit messages after that Seq; otherwise
// the last limit messages before before (0: the end). limit 0 means 50.
func (n *Node) ChatMessages(id string, before, after uint64, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	var all []ChatMessage
	if root, peer, ok := parseLegacyChatID(id); ok {
		chats, err := n.legacyChats()
		if err != nil {
			return nil, err
		}
		found := false
		for _, lc := range chats {
			if lc.root == root && lc.peer == peer {
				all, found = lc.msgs, true
			}
		}
		if !found {
			return nil, fmt.Errorf("%w %s", ErrUnknownChat, id)
		}
	} else {
		s, ok := n.chats.snapshot(id)
		if !ok {
			return nil, fmt.Errorf("%w %s", ErrUnknownChat, id)
		}
		all = make([]ChatMessage, 0, len(s.msgs))
		for _, r := range s.msgs {
			all = append(all, n.chatMessage(s.chat, r))
		}
	}
	out := []ChatMessage{}
	if after > 0 {
		for _, m := range all {
			if m.Seq > after && len(out) < limit {
				out = append(out, m)
			}
		}
		return out, nil
	}
	for _, m := range all {
		if before == 0 || m.Seq < before {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (n *Node) chatMessage(c Chat, r chatRecord) ChatMessage {
	cm := ChatMessage{Seq: r.Seq, Direction: "in", Held: r.Message.Held(), Message: r.Message}
	if r.Message.From == n.cfg.Node {
		cm.Direction = "out"
		for _, p := range c.Participants {
			if p == n.cfg.Node {
				continue
			}
			status := n.store.delivery(p, r.Message.ID)
			if status == "" {
				status = "queued"
			}
			cm.Delivery = append(cm.Delivery, Delivery{Peer: p, Status: status})
		}
	}
	return cm
}

// queuedByChat counts, per peer and chat, the chat messages still in the outbox.
func (n *Node) queuedByChat() map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, p := range n.Peers() {
		msgs, err := n.store.pending(p)
		if err != nil {
			n.log.Warn("read outbox", "peer", p, "err", err)
			continue
		}
		for _, m := range msgs {
			if m.ChatID == "" || m.Kind == KindStatus {
				continue
			}
			if out[p] == nil {
				out[p] = map[string]int{}
			}
			out[p][m.ChatID]++
		}
	}
	return out
}

func (n *Node) chatInfo(s chatSnapshot, queued map[string]map[string]int) ChatInfo {
	info := ChatInfo{Chat: s.chat, Closed: s.chat.Closed(), LastAt: s.chat.CreatedAt}
	var last *chatRecord
	for i, r := range s.msgs {
		info.LastSeq = r.Seq
		if r.Message.Kind != "" {
			continue
		}
		if info.Count == 0 {
			info.Title = title(r.Message.Body)
		}
		info.Count++
		last = &s.msgs[i]
	}
	if last != nil {
		cm := n.chatMessage(s.chat, *last)
		info.LastMessage, info.LastAt = &cm, last.Message.CreatedAt
	}
	info.Archived = info.Closed
	jobs := map[string][]JobActivity{}
	for _, m := range s.jobs {
		if m.JobStatus != JobQueued && m.JobStatus != JobRunning {
			continue
		}
		jobs[m.From] = append(jobs[m.From], JobActivity{ReplyTo: m.ReplyTo, JobStatus: m.JobStatus,
			Activity: m.Activity, ActivityInfo: m.ActivityInfo, UpdatedAt: m.CreatedAt})
	}
	for _, p := range s.chat.Participants {
		ps := ParticipantState{Name: p, Self: p == n.cfg.Node, Connected: true, Compatible: true, Jobs: jobs[p]}
		if !ps.Self {
			_, caps, ok := n.PeerCaps(p)
			ps.Connected = ok
			ps.Compatible = !ok || slices.Contains(caps, CapChat)
			ps.Queued = queued[p][s.chat.ID]
			for i := range ps.Jobs {
				ps.Jobs[i].Stale = !ok
			}
		}
		info.Active = info.Active || len(ps.Jobs) > 0
		info.Members = append(info.Members, ps)
	}
	return info
}

// legacyArchived reports whether this node moved a legacy chat to the archive.
func legacyArchived(cs *chatStore, id string) bool {
	v, ok := cs.view(id)
	return ok && v.Archived
}

// title is the first line of body, at most 80 characters.
func title(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > 80 {
		r := []rune(line)
		line = string(r[:79]) + "…"
	}
	return line
}

// legacyChat is one virtual chat of pre-chat history.
type legacyChat struct {
	root, peer string
	info       ChatInfo
	msgs       []ChatMessage
}

// legacyChats groups history from before chats (Recent) into virtual chats:
// one per peer and area (the area of the request a message leads back to), in
// time order, so an area request gets one chat for each member it went to and
// every plain exchange with one member reads as one conversation. The chat's
// id names its oldest request.
func (n *Node) legacyChats() ([]legacyChat, error) {
	entries, err := n.Recent(0)
	if err != nil {
		return nil, err
	}
	parent, area := map[string]string{}, map[string]string{}
	for _, e := range entries {
		parent[e.ID] = e.ReplyTo
		if e.Area != "" {
			area[e.ID] = e.Area
		}
	}
	rootOf := func(id string) string {
		for range 64 { // guards against a reply_to cycle
			p, ok := parent[id]
			if !ok || p == "" {
				return id
			}
			id = p
		}
		return id
	}
	type group struct {
		root, peer string
		entries    []Entry
	}
	groups := map[string]*group{}
	for _, e := range entries {
		if e.Kind != "" || e.Peer == "" {
			continue
		}
		key := e.Peer + "\x00" + area[rootOf(e.ID)]
		g := groups[key]
		if g == nil {
			g = &group{peer: e.Peer}
			groups[key] = g
		}
		g.entries = append(g.entries, e)
	}
	out := make([]legacyChat, 0, len(groups))
	for _, g := range groups {
		slices.SortFunc(g.entries, func(a, b Entry) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
		})
		g.root = rootOf(g.entries[0].ID)
		id := legacyChatID(g.root, g.peer)
		parts := []string{n.cfg.Node, g.peer}
		slices.Sort(parts)
		lc := legacyChat{root: g.root, peer: g.peer}
		info := ChatInfo{ID: id, Participants: parts, CreatedAt: g.entries[0].CreatedAt, Legacy: true, Peer: g.peer}
		var job, ownJob *JobActivity // the peer's job on our request, ours on its request
		for i, e := range g.entries {
			m := e.Message
			if info.Area == "" {
				info.Area = m.Area
			}
			cm := ChatMessage{Seq: uint64(i + 1), Direction: e.Direction, Message: m}
			if e.IsRequest() {
				// A request's folded job state goes to its participant's jobs;
				// the message itself shows what was said, never a failure.
				cm.JobStatus = ""
			}
			busy := e.IsRequest() && e.Answer == "" && (e.JobStatus == JobQueued || e.JobStatus == JobRunning)
			if e.Direction == "out" {
				cm.Delivery = []Delivery{{Peer: g.peer, Status: e.Status}}
				if busy {
					job = &JobActivity{ReplyTo: e.ID, JobStatus: e.JobStatus, Activity: e.Activity, UpdatedAt: e.LastHeard}
				}
			} else if busy {
				ownJob = &JobActivity{ReplyTo: e.ID, JobStatus: e.JobStatus, Activity: e.Activity, UpdatedAt: e.CreatedAt}
			}
			lc.msgs = append(lc.msgs, cm)
		}
		info.Title = title(g.entries[0].Body)
		info.Count = len(lc.msgs)
		info.LastSeq = uint64(len(lc.msgs))
		last := lc.msgs[len(lc.msgs)-1]
		info.LastMessage, info.LastAt = &last, last.CreatedAt
		info.Archived = legacyArchived(n.chats, id)
		_, caps, connected := n.PeerCaps(g.peer)
		peerState := ParticipantState{Name: g.peer, Connected: connected, Compatible: !connected || slices.Contains(caps, CapChat)}
		if job != nil {
			job.Stale = !connected
			peerState.Jobs = []JobActivity{*job}
			info.Active = true
		}
		selfState := ParticipantState{Name: n.cfg.Node, Self: true, Connected: true, Compatible: true}
		if ownJob != nil {
			selfState.Jobs = []JobActivity{*ownJob}
			info.Active = true
		}
		for _, p := range parts {
			if p == n.cfg.Node {
				info.Members = append(info.Members, selfState)
			} else {
				info.Members = append(info.Members, peerState)
			}
		}
		lc.info = info
		out = append(out, lc)
	}
	return out, nil
}
