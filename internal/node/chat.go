package node

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	// Gen is the generation of a keyed chat (0 for the first one and for the
	// random-id chats of v0.5).
	Gen uint32 `json:"gen,omitempty"`
	// Project is the project id of a chat in a project node; "" in the
	// legacy network.
	Project string `json:"project,omitempty"`
	// Mode is ChatModeProject for a standalone project chat, "" otherwise.
	Mode string `json:"mode,omitempty"`
	// ParticipantIDs pins each of Participants (same order) to a node id in
	// project chats.
	ParticipantIDs []string `json:"participant_ids,omitempty"`
}

// ChatModeProject marks a standalone project chat (Chat.Mode, Message.ChatMode).
const ChatModeProject = "project"

// Closed reports whether a participant closed the chat.
func (c Chat) Closed() bool { return c.CloseID != "" }

// Keyed reports whether c is the chat of its conversation key and generation
// (KeyedChatID, ProjectChatID in a project), not a random-id chat of an older
// version or a standalone project chat.
func (c Chat) Keyed() bool {
	if c.Project != "" {
		return c.Mode == "" && len(c.ParticipantIDs) == len(c.Participants) &&
			c.ID == ProjectChatID(c.Project, c.Participants, c.ParticipantIDs, c.Gen)
	}
	return c.ID == KeyedChatID(c.Area, c.Participants, c.Gen)
}

// key is c's conversation key: the pinned members in a project, else the
// area and the participants.
func (c Chat) key() string {
	if c.Project != "" {
		return projectChatKey(c.Project, c.Participants, c.ParticipantIDs)
	}
	return chatKey(c.Area, c.Participants)
}

// stamp puts c's envelope fields on m, a message of c.
func (c Chat) stamp(m *Message) {
	m.ChatID, m.Participants, m.Area, m.ChatGen = c.ID, c.Participants, c.Area, c.Gen
	m.ParticipantIDs, m.ChatMode = c.ParticipantIDs, c.Mode
}

// chatKey is the conversation key K of an area and a sorted participant list.
func chatKey(area string, parts []string) string { return area + "\x00" + strings.Join(parts, ",") }

// projectChatKey is the conversation key of a project's keyed chat: the
// project and its members as sorted name@nodeid entries, so a member who
// re-joins with a new node id starts new conversations.
func projectChatKey(pid string, parts, ids []string) string {
	members := make([]string, len(parts))
	for i, p := range parts {
		if i < len(ids) {
			members[i] = p + "@" + ids[i]
		}
	}
	slices.Sort(members)
	return pid + "\x00" + strings.Join(members, ",")
}

// ProjectChatID is KeyedChatID in a project: the id of generation gen of the
// one chat of the participants, pinned to their node ids (ids, aligned with
// the sorted parts). Ids of equal names differ between projects.
func ProjectChatID(pid string, parts, ids []string, gen uint32) string {
	return DerivedID("agentlink-chat-v3/"+projectChatKey(pid, parts, ids), strconv.FormatUint(uint64(gen), 10))
}

// KeyedChatID is the id of generation gen of the one chat of area and the
// sorted participants (this node included): every node computes the same id,
// so two nodes that start the conversation at once get one chat. A person's
// close starts the next generation.
func KeyedChatID(area string, parts []string, gen uint32) string {
	return DerivedID("agentlink-chat-v2\x00"+chatKey(area, parts), strconv.FormatUint(uint64(gen), 10))
}

// ChatView is this node's view of a legacy chat (local history): archived when
// a side closed it (By), up to At; a later message brings it back. A real chat
// is archived exactly when it is closed.
type ChatView struct {
	Archived bool      `json:"archived"`
	At       time.Time `json:"at,omitzero"`
	By       string    `json:"by,omitempty"`
}

// Delivery is where one copy of an outbound chat message is. Status is the
// transport: "queued" until the peer ACKs it, then "sent". State is the
// recipient's progress: queued, delivered, read, answered (State* constants),
// At when it reached the read or answered state.
type Delivery struct {
	Peer   string    `json:"peer"`
	Status string    `json:"status"`
	State  string    `json:"state"`
	At     time.Time `json:"at,omitzero"`
}

// ChatMessage is one message of a chat as listed by the chat API: one entry
// per message id, however many peers it went to. Seq is this node's order.
type ChatMessage struct {
	Seq       uint64     `json:"seq"`
	Direction string     `json:"direction"` // "in" or "out"
	Held      bool       `json:"held,omitempty"`
	Delivery  []Delivery `json:"delivery,omitempty"` // out only
	// Unread: this node's sessions have not acknowledged it (see Ack). A
	// browser showing it does not change that.
	Unread bool `json:"unread,omitempty"`
	// OwnHuman: a person on this node wrote it (to the others); it is unread
	// for this node's sessions as information only, never a request.
	OwnHuman bool `json:"own_human,omitempty"`
	// Assigned: who on this node answers it ("worker", "session:<id>").
	Assigned string `json:"assigned,omitempty"`
	Message
}

// JobActivity is a job a participant runs or queues for a chat request.
type JobActivity struct {
	ReplyTo      string         `json:"reply_to"`
	JobStatus    string         `json:"job_status"`
	Activity     string         `json:"activity,omitempty"`
	ActivityInfo *ActivityState `json:"activity_info,omitempty"`
	HoldReason   string         `json:"hold_reason,omitempty"` // held only
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
	// Held are the requests the participant was asked but will not answer
	// automatically (JobHeld, with HoldReason and HoldText as Activity), until
	// it answers them. They do not make the chat active.
	Held []JobActivity `json:"held,omitempty"`
	// Presence is what the connected participant says of its session for the
	// chat's area (PeerPresence); absent when unknown.
	Presence *AreaPresence `json:"presence,omitempty"`
}

// ChatInfo describes a chat for the chat list and the chat page.
type ChatInfo struct {
	Chat
	Closed   bool `json:"closed"`
	Archived bool `json:"archived"`
	// Legacy marks a virtual chat built from history before chats: one
	// request with its replies, with one peer. Sending to it continues it in a
	// real chat (continueLegacy); closing it archives it on both sides (on
	// this node only when the peer has no chats), ClosedBy and ClosedAt then
	// say who and when, and Closed stays false.
	Legacy bool   `json:"legacy,omitempty"`
	Peer   string `json:"peer,omitempty"` // legacy only: the other side
	Title  string `json:"title"`          // the first message, shortened
	// Keyed: the one chat of its conversation key (KeyedChatID); a random-id
	// chat of v0.5 is kept as archived history.
	Keyed bool `json:"keyed,omitempty"`
	// Unread counts the messages this node's sessions have not acknowledged.
	Unread int `json:"unread,omitempty"`
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

// CreateChat is EnsureOpenChat for the members in with (names, also
// comma-separated): the open chat of this node, them and area.
func (n *Node) CreateChat(with []string, area string) (ChatInfo, error) {
	parts, err := n.normalizeParticipants(with)
	if err != nil {
		return ChatInfo{}, err
	}
	c, err := n.EnsureOpenChat(parts, area)
	if err != nil {
		return ChatInfo{}, err
	}
	return n.Chat(c.ID)
}

// EnsureOpenChat returns the open chat of conversation key (area, parts):
// parts are all participants, sorted, this node included. It is the keyed
// chat of the key's open generation, created (and announced) when this node
// does not have it yet. Every node computes the same id, so a conversation
// started on two nodes at once is one chat. Every participant must be a known
// member; one connected now without chat support cannot take part
// (ErrNoChatSupport). A disconnected one gets the chat when it connects.
func (n *Node) EnsureOpenChat(parts []string, area string) (Chat, error) {
	if len(parts) < 2 || !slices.IsSorted(parts) || !slices.Contains(parts, n.cfg.Node) ||
		len(slices.Compact(slices.Clone(parts))) != len(parts) {
		return Chat{}, fmt.Errorf("%w: name at least one other member", ErrBadParticipants)
	}
	if area != "" && (!config.ValidName(area) || n.cfg.Project != "") {
		return Chat{}, fmt.Errorf("invalid area %q", area)
	}
	n.ensureMu.Lock()
	defer n.ensureMu.Unlock()
	proto := Chat{Participants: parts, Area: area, Project: n.cfg.Project}
	if proto.Project != "" {
		ids, err := n.pinIDs(parts)
		if err != nil {
			return Chat{}, err
		}
		proto.ParticipantIDs = ids
	}
	key := proto.key()
	for {
		gen := n.chats.gen(key)
		id := KeyedChatID(area, parts, gen)
		if proto.Project != "" {
			id = ProjectChatID(proto.Project, parts, proto.ParticipantIDs, gen)
		}
		c, ok := n.chats.get(id)
		if ok && !c.Closed() {
			return c, nil
		}
		if ok { // closed: the generation has moved on (noteGenLocked)
			if n.chats.gen(key) == gen {
				return Chat{}, fmt.Errorf("%w %s", ErrChatClosed, id)
			}
			continue
		}
		var missing []string
		for _, p := range parts {
			if p == n.cfg.Node {
				continue
			}
			n.mu.Lock()
			known := n.known[p] && !n.removedLocked(p)
			n.mu.Unlock()
			if !known {
				return Chat{}, fmt.Errorf("%w %q", ErrUnknownPeer, p)
			}
			if n.Connected(p) && !n.PeerHas(p, CapChat) {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			return Chat{}, fmt.Errorf("%w: %s", ErrNoChatSupport, strings.Join(missing, ", "))
		}
		c = proto
		c.ID, c.Gen, c.CreatedAt = id, gen, time.Now().UTC()
		if _, err := n.chats.ensure(c); err != nil {
			return Chat{}, err
		}
		open := Message{ID: DerivedID(c.ID, "open"), Kind: KindChatOpen, CreatedAt: c.CreatedAt}
		if err := n.postChat(c, open); err != nil {
			return Chat{}, err
		}
		n.changed("chats")
		return c, nil
	}
}

// openChatOf returns the open chat of c's conversation: c itself when it is
// an open keyed chat, else the open generation of its key (a closed chat, a
// random-id chat of v0.5).
func (n *Node) openChatOf(c Chat) (Chat, error) {
	if c.Keyed() && !c.Closed() {
		return c, nil
	}
	return n.EnsureOpenChat(c.Participants, c.Area)
}

// CloseChat closes a chat for every participant, which moves it to the
// archive on every node: a chat is archived exactly when it is closed, and
// only people close chats (the app; the control API has no close). Closing a
// closed chat is a no-op. The next message of the conversation opens its next
// generation. Jobs already running finish; their replies are still kept. A
// legacy chat is closed with closeLegacy.
func (n *Node) CloseChat(id string) (ChatInfo, error) {
	if _, peer, ok := parseLegacyChatID(id); ok {
		return n.closeLegacy(id, peer)
	}
	c, ok := n.chats.get(id)
	if !ok {
		return ChatInfo{}, fmt.Errorf("%w %s", ErrUnknownChat, id)
	}
	if !c.Closed() {
		m := Message{ID: DerivedID(c.ID, "close/"+n.cfg.Node), Kind: KindChatClose, AuthorKind: AuthorHuman, CreatedAt: time.Now().UTC()}
		if err := n.postChat(c, m); err != nil {
			return ChatInfo{}, err
		}
		n.changed("chats")
	}
	return n.Chat(id)
}

// closeLegacy archives legacy chat id with peer here, up to its last message
// or now, whichever is later, and tells the peer: a chat_close whose ChatID is
// this legacy id and whose Area is the chat's area. The outbox only sends it
// to a peer with chats (now or once it connects with them), so a peer
// connected without chat support is not told at all.
func (n *Node) closeLegacy(id, peer string) (ChatInfo, error) {
	info, err := n.Chat(id)
	if err != nil {
		return ChatInfo{}, err
	}
	at := time.Now().UTC()
	if info.LastAt.After(at) {
		at = info.LastAt
	}
	if err := n.chats.setView(id, ChatView{Archived: true, At: at, By: n.cfg.Node}); err != nil {
		return ChatInfo{}, err
	}
	if !n.Connected(peer) || n.PeerHas(peer, CapChat) {
		m := Message{ID: newID(), From: n.cfg.Node, To: peer, ChatID: id, Kind: KindChatClose, Area: info.Area, CreatedAt: at}
		if err := n.enqueue(peer, m); err != nil {
			return ChatInfo{}, err
		}
	}
	n.changed("chats")
	return n.Chat(id)
}

// receiveLegacyClose archives, for peer's chat_close of its legacy chat, this
// node's legacy chat with peer of the same area up to the close time. It
// reports false for an invalid message.
func (n *Node) receiveLegacyClose(peer string, m Message) bool {
	if m.Kind != KindChatClose || (m.Area != "" && !config.ValidName(m.Area)) {
		return false
	}
	chats, err := n.legacyChats()
	if err != nil {
		n.log.Warn("legacy chat close", "peer", peer, "err", err)
		return true
	}
	for _, lc := range chats {
		if lc.peer != peer || lc.info.Area != m.Area {
			continue
		}
		if v, ok := n.chats.view(lc.info.ID); ok && v.Archived && !v.At.Before(m.CreatedAt) {
			continue // a duplicate or an older close
		}
		if err := n.chats.setView(lc.info.ID, ChatView{Archived: true, At: m.CreatedAt, By: peer}); err != nil {
			n.log.Warn("legacy chat close", "peer", peer, "err", err)
			continue
		}
		n.changed("chats")
	}
	return true
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
	// AuthorKind is who writes it (Author*); empty means an agent.
	AuthorKind string
}

// SendChat posts a message to a chat. Without Ask it only informs. A closed
// chat or a random-id chat of v0.5 is continued in the open chat of its
// conversation (openChatOf), a legacy chat by continueLegacy.
func (n *Node) SendChat(s ChatSend) (Message, error) {
	if _, peer, ok := parseLegacyChatID(s.ChatID); ok {
		return n.continueLegacy(peer, s)
	}
	switch s.AuthorKind {
	case "":
		s.AuthorKind = AuthorAgent
	case AuthorHuman, AuthorAgent, AuthorWorker:
	default:
		return Message{}, fmt.Errorf("invalid author_kind %q", s.AuthorKind)
	}
	if c, ok := n.chats.get(s.ChatID); ok {
		open, err := n.openChatOf(c)
		if err != nil {
			return Message{}, err
		}
		s.ChatID = open.ID
	}
	m := Message{ChatID: s.ChatID, Body: s.Body, ReplyTo: s.ReplyTo, AuthorKind: s.AuthorKind}
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
	parts := []string{n.cfg.Node, peer}
	slices.Sort(parts)
	open, err := n.EnsureOpenChat(parts, legacy.Area)
	if err != nil {
		return Message{}, err
	}
	s.ChatID, s.ReplyTo = open.ID, ""
	return n.SendChat(s)
}

// inheritChain sets m's automatic chain (RootID, AutoDepth) when it has none
// yet. A person's message starts a new chain at itself. Any other message
// continues the chain of its base, one hop further: the message it replies to,
// else the newest message of chat c from another node or by a person here. So the depth counts
// the agent hops since a person last wrote, whichever nodes and sessions the
// agents run in, and MaxAutoDepth bounds a conversation of agents alone.
func (n *Node) inheritChain(c Chat, m *Message) {
	if m.RootID != "" {
		return
	}
	m.RootID, m.AutoDepth = m.ID, 0
	if m.AuthorKind == AuthorHuman {
		return
	}
	var base *Message
	if m.ReplyTo != "" {
		if r, ok := n.chats.message(m.ReplyTo); ok && r.Message.Kind == "" {
			base = &r.Message
		}
	}
	if base == nil {
		if s, ok := n.chats.snapshot(c.ID); ok {
			for _, rec := range slices.Backward(s.msgs) {
				if r := rec.Message; r.Kind == "" && r.ID != m.ID && (r.From != n.cfg.Node || r.AuthorKind == AuthorHuman) {
					base = &r
					break
				}
			}
		}
	}
	if base != nil {
		m.RootID, m.AutoDepth = cmp.Or(base.RootID, base.ID), base.AutoDepth
		if m.AutoDepth < 255 {
			m.AutoDepth++
		}
	}
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
		n.inheritChain(c, &m)
	} else {
		m.CreatedAt = time.Now().UTC()
	}
	if err := n.postChat(c, m); err != nil {
		return Message{}, err
	}
	m.From = n.cfg.Node
	c.stamp(&m)
	return m, nil
}

// postChat stores m in chat c, then queues a copy for every other participant.
// A status update is only kept as its job's latest status.
// A person's message is also unread here: this node's own sessions learn what
// their person told the others (ChatMessage.OwnHuman).
func (n *Node) postChat(c Chat, m Message) error {
	m.From, m.To = n.cfg.Node, ""
	c.stamp(&m)
	if m.Kind == KindStatus {
		n.chats.noteStatus(m)
	} else if _, isNew, _, err := n.chats.put(m, m.AuthorKind == AuthorHuman); err != nil {
		return err
	} else if isNew && m.Kind == "" && m.ReplyTo != "" {
		// Replying reads what it answers: the reply tells its author.
		if _, _, err := n.chats.markRead(m.ReplyTo, "", n.cfg.Node); err != nil {
			n.log.Warn("mark replied message read", "id", m.ReplyTo, "err", err)
		}
	}
	return n.fanout(c, m, false)
}

// fanout queues m for every participant but this node. With missingOnly only
// peers that have no copy queued or ACKed get one (recovery after a crash).
// In a project a participant whose member record no longer has the node id
// the chat pins it to (it left, or re-joined as a new node) gets nothing.
func (n *Node) fanout(c Chat, m Message, missingOnly bool) error {
	for i, p := range c.Participants {
		if p == n.cfg.Node || (missingOnly && n.store.delivery(p, m.ID) != "") || !n.pinned(c, i) {
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
			for i, p := range s.chat.Participants {
				if p == n.cfg.Node || n.store.delivery(p, r.Message.ID) != "" || !n.pinned(s.chat, i) {
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

// pinIDs returns the node ids of parts (sorted, this node included) as the
// member table has them now, for a new project chat: every participant must
// be a live member with a node id.
func (n *Node) pinIDs(parts []string) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := make([]string, len(parts))
	for i, p := range parts {
		if p == n.cfg.Node {
			ids[i] = n.id
			continue
		}
		m := n.members[p]
		if m == nil || m.Removed || !validID(m.ID) {
			return nil, fmt.Errorf("%w %q", ErrUnknownPeer, p)
		}
		ids[i] = m.ID
	}
	return ids, nil
}

// pinned reports whether participant i of c is still the node the chat was
// made with: always outside projects; in a project, while the member record
// of that name has the pinned node id and is not removed.
func (n *Node) pinned(c Chat, i int) bool {
	if c.Project == "" {
		return true
	}
	if i >= len(c.ParticipantIDs) {
		return false
	}
	if c.Participants[i] == n.cfg.Node {
		return c.ParticipantIDs[i] == n.id
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	m := n.members[c.Participants[i]]
	return m != nil && !m.Removed && m.ID == c.ParticipantIDs[i]
}

// validProjectEnvelope checks the project fields of a chat message from
// peer, whose session authenticated node id peerID: every participant pinned
// to a valid node id, the author to peerID and this node to its own id; no
// area; a keyed chat's id must be its ProjectChatID. The legacy network takes
// none of these fields.
func (n *Node) validProjectEnvelope(peer, peerID string, m *Message) bool {
	if n.cfg.Project == "" {
		return len(m.ParticipantIDs) == 0 && m.ChatMode == ""
	}
	p, ids := m.Participants, m.ParticipantIDs
	if m.Area != "" || len(ids) != len(p) {
		return false
	}
	for i, id := range ids {
		switch {
		case !validID(id):
			return false
		case p[i] == peer && id != peerID:
			return false
		case p[i] == n.cfg.Node && id != n.id:
			return false
		}
	}
	return m.ChatMode == "" && m.ChatID == ProjectChatID(n.cfg.Project, p, ids, m.ChatGen)
}

// validChatEnvelope checks a chat message from peer: a known kind, the full
// participant list with the author and this node, and responders among them.
func (n *Node) validChatEnvelope(peer, peerID string, m *Message) bool {
	if !n.validProjectEnvelope(peer, peerID, m) {
		return false
	}
	switch m.Kind {
	case "", KindStatus, KindChatOpen, KindChatClose, KindReceipt:
	default:
		return false
	}
	switch m.AuthorKind {
	case "", AuthorHuman, AuthorAgent, AuthorWorker:
	default:
		m.AuthorKind = "" // a newer kind: shown as unknown
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
func (n *Node) receiveChat(peer, peerID string, m Message) bool {
	if !n.validChatEnvelope(peer, peerID, &m) {
		return false
	}
	created, err := n.chats.ensure(Chat{ID: m.ChatID, Participants: m.Participants, Area: m.Area, Gen: m.ChatGen, CreatedAt: m.CreatedAt,
		Project: n.cfg.Project, Mode: m.ChatMode, ParticipantIDs: m.ParticipantIDs})
	if err != nil {
		n.log.Warn("chat message rejected", "peer", peer, "chat", m.ChatID, "err", err)
		return false
	}
	closed := false
	switch m.Kind {
	case KindReceipt:
		n.receiveReceipts(peer, m)
	case KindStatus:
		if n.chats.noteStatus(m) {
			n.changed("messages")
		}
	default:
		_, isNew, c, err := n.chats.put(m, true)
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

// WorkerOwner is the Assigned value of a request the worker answers.
const WorkerOwner = "worker"

// ClaimRun reports whether this node's worker should answer m and, if so,
// assigns m to it and marks it read (a read receipt goes to its author): m
// must ask this node, the chat be open, m within MaxAutoDepth, no live
// session be registered for m's area (LiveSession) and nobody else be
// assigned or have read it. The assignment is atomic with Ack, so a request
// is handled by the worker or by a session, never both. When m asks this node
// but must not run, hold is the reason (a Hold* constant) or "" when a session
// takes it. Asking the same m again gives the same answer, so a resent
// duplicate is safe.
func (n *Node) ClaimRun(m Message) (run bool, hold string, err error) {
	if !m.Asks(n.cfg.Node) {
		return false, "", nil
	}
	if m.Held() {
		return false, HoldAutoLimit, nil
	}
	c, ok := n.chats.get(m.ChatID)
	if !ok || c.Closed() {
		return false, HoldChatClosed, nil
	}
	r, ok := n.chats.message(m.ID)
	if !ok {
		return false, "", nil // not stored here (yet)
	}
	if r.Assigned == WorkerOwner {
		return true, "", nil
	}
	if n.LiveSession(c.Area) {
		return false, "", nil
	}
	ok, wasUnread, err := n.chats.claim(m.ID, WorkerOwner)
	if err != nil || !ok {
		return false, "", err
	}
	if wasUnread {
		n.sendReceipts(map[string][]string{r.Message.From: {m.ID}}, StateRead)
		n.changed("messages")
	}
	return true, "", nil
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
	cm := ChatMessage{Seq: r.Seq, Direction: "in", Held: r.Message.Held(), Message: r.Message,
		Unread: r.Unread && r.ReadAt.IsZero(), Assigned: r.Assigned}
	if r.Message.From == n.cfg.Node {
		cm.Direction = "out"
		cm.OwnHuman = r.Message.AuthorKind == AuthorHuman
		if r.Message.Kind != "" {
			return cm
		}
		replied := n.chats.repliedBy(r.Message.ID)
		for i, p := range c.Participants {
			if p == n.cfg.Node {
				continue
			}
			d := Delivery{Peer: p, Status: n.store.delivery(p, r.Message.ID), State: StateQueued}
			switch {
			case d.Status == "sent":
				d.State = StateDelivered
			case !n.pinned(c, i):
				d.Status, d.State = "queued", StateLeft // never sent: the pinned node is gone
			case d.Status == "":
				d.Status = "queued"
			}
			if rc, ok := r.Receipts[p]; ok && stateRank(rc.State) > stateRank(d.State) {
				d.State, d.At = rc.State, rc.At
			}
			if at, ok := replied[p]; ok {
				d.State, d.At = StateAnswered, at
			}
			cm.Delivery = append(cm.Delivery, d)
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
			if m.ChatID == "" || m.Kind == KindStatus || m.Kind == KindReceipt {
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
	info.Keyed = s.chat.Keyed()
	var last *chatRecord
	for i, r := range s.msgs {
		info.LastSeq = r.Seq
		if r.Message.Kind != "" {
			continue
		}
		if r.Unread && r.ReadAt.IsZero() {
			info.Unread++
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
	// A random-id chat of v0.5 is history: its conversation goes on in the
	// keyed chat (openChatOf).
	info.Archived = info.Closed || !info.Keyed
	jobs, held := map[string][]JobActivity{}, map[string][]JobActivity{}
	for _, m := range s.jobs {
		a := JobActivity{ReplyTo: m.ReplyTo, JobStatus: m.JobStatus, Activity: m.Activity, ActivityInfo: m.ActivityInfo, UpdatedAt: m.CreatedAt}
		switch m.JobStatus {
		case JobQueued, JobRunning:
			jobs[m.From] = append(jobs[m.From], a)
		case JobHeld:
			a.HoldReason, a.ActivityInfo = m.HoldReason, nil
			if a.Activity == "" {
				a.Activity = HoldText(m.HoldReason)
			}
			held[m.From] = append(held[m.From], a)
		}
	}
	for _, p := range s.chat.Participants {
		ps := ParticipantState{Name: p, Self: p == n.cfg.Node, Connected: true, Compatible: true, Jobs: jobs[p], Held: held[p]}
		if !ps.Self {
			_, caps, ok := n.PeerCaps(p)
			ps.Connected = ok
			ps.Compatible = !ok || slices.Contains(caps, CapChat)
			ps.Queued = queued[p][s.chat.ID]
			for i := range ps.Jobs {
				ps.Jobs[i].Stale = !ok
			}
			if pr, known := n.PeerPresence(p, s.chat.Area); known {
				ps.Presence = &pr
			}
		}
		info.Active = info.Active || len(ps.Jobs) > 0
		info.Members = append(info.Members, ps)
	}
	return info
}

// legacyView reports whether a legacy chat whose last message is at last is
// archived, and by whom: a message after the close brings it back.
func legacyView(cs *chatStore, id string, last time.Time) (ChatView, bool) {
	v, ok := cs.view(id)
	return v, ok && v.Archived && !last.After(v.At)
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
		if v, archived := legacyView(n.chats, id, info.LastAt); archived {
			info.Archived, info.ClosedBy, info.ClosedAt = true, cmp.Or(v.By, n.cfg.Node), v.At
		}
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
