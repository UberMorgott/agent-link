package node

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"
)

// AreaPrefix marks an area address in Message.To, e.g. "area:dev".
const AreaPrefix = "area:"

// KindStatus marks a job status update: a bodiless message whose ReplyTo is
// the request and whose JobStatus is queued or running. Status updates never
// wake wait and never start a handler.
const KindStatus = "status"

// Chat control kinds: KindChatOpen announces a chat to its participants,
// KindChatClose closes it. Neither starts a handler nor wakes wait.
const (
	KindChatOpen  = "chat_open"
	KindChatClose = "chat_close"
	// KindReceipt carries read receipts (Receipts) of one reader node back to
	// the author of the messages, to peers with CapReceipts only. It is never
	// stored as a chat message and never wakes anything.
	KindReceipt = "receipt"
	// KindChatMembers changes who takes part in a standalone project chat: its
	// envelope carries the new Participants and a higher ChatRev. Only the
	// chat's owner sends it, to peers with CapChatMembers, the removed ones too.
	KindChatMembers = "chat_members"
)

// Author kinds carried in Message.AuthorKind: who wrote a message. A person
// writes in the app (the chat composer), an agent through the CLI or a hook,
// the worker its automatic answers. Older peers send none.
const (
	AuthorHuman  = "human"
	AuthorAgent  = "agent"
	AuthorWorker = "worker"
)

// Per-recipient delivery states of a chat message (Delivery.State), in order.
const (
	StateQueued    = "queued"    // in this node's outbox
	StateDelivered = "delivered" // the recipient node stored it (ACK)
	StateRead      = "read"      // a session or the worker there acknowledged it
	StateAnswered  = "answered"  // the recipient replied to it (reply_to)
	// StateLeft: a project chat's recipient is no longer the node the chat
	// pins (it left, or re-joined with a new node id); nothing goes to it.
	StateLeft = "left"
)

// Receipt is one message's state at the reader node, on KindReceipt messages.
type Receipt struct {
	ID    string    `json:"id"`
	State string    `json:"state"` // StateRead or StateAnswered
	At    time.Time `json:"at"`
}

func stateRank(s string) int {
	switch s {
	case StateDelivered:
		return 1
	case StateRead:
		return 2
	case StateAnswered:
		return 3
	}
	return 0
}

// Activity phases carried in ActivityState.Phase.
const (
	PhaseRunning = "running"
	PhaseDone    = "done"
)

// ActivityState is one operation a running handler reports (a tool call, a
// command, thinking), on status updates of chat jobs. ID ties the start and
// the end of one operation; Seq orders the updates of one job.
type ActivityState struct {
	ID        string    `json:"id,omitempty"`
	Type      string    `json:"type,omitempty"` // e.g. "thinking", "edit", "command", "read", "search", "tool"
	Text      string    `json:"text,omitempty"`
	Phase     string    `json:"phase,omitempty"` // PhaseRunning or PhaseDone
	StartedAt time.Time `json:"started_at,omitzero"`
	Seq       uint64    `json:"seq,omitempty"`
	// Session names the live session reporting (a short id), so several
	// sessions of one node show one line each; empty for the worker. Older
	// peers ignore it and show one line per node and request.
	Session string `json:"session,omitempty"`
}

// Job statuses carried in Message.JobStatus.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
	// JobHeld is a terminal status of a chat request this node was asked but
	// will not answer automatically; HoldReason says why. Peers without it
	// ignore the status (it is neither queued nor running).
	JobHeld = "held"
)

// Hold reasons carried in Message.HoldReason.
const (
	HoldNoHandler  = "no_handler"  // no agent handler is configured
	HoldNoAgent    = "no_agent"    // the handler's agent program is not found
	HoldAutoLimit  = "auto_limit"  // automatic chain limit, or its root already ran here
	HoldChatClosed = "chat_closed" // the chat is closed (or gone)
	HoldAnswered   = "answered"    // a person on this node answered it
	HoldNoFolder   = "no_folder"   // a project member has no folder bound: no agent runs
)

// HoldText is the text shown for a hold reason.
func HoldText(reason string) string {
	switch reason {
	case HoldNoHandler:
		return "никто не отвечает — ждёт человека"
	case HoldNoAgent:
		return "программа агента не найдена — ждёт человека"
	case HoldAutoLimit:
		return "пауза — нужен человек"
	case HoldChatClosed:
		return "чат закрыт"
	case HoldAnswered:
		return "ответил человек"
	case HoldNoFolder:
		return "у участника не выбрана папка проекта"
	}
	return "не отвечает автоматически — ждёт человека"
}

// Message is one agent-to-agent message.
type Message struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Area      string `json:"area,omitempty"`
	Body      string `json:"body"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Kind      string `json:"kind,omitempty"`       // "" (a message) or KindStatus
	JobStatus string `json:"job_status,omitempty"` // on status updates and handler replies
	// Activity is what a running handler is doing now ("Read docs/index.md"),
	// on running status updates only. Older peers neither send nor read it.
	Activity  string    `json:"activity,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Chat fields, only on messages of a chat (peers with CapChat). Every
	// envelope carries the chat's full, fixed Participants (sorted, author
	// included) and its Area, so a receiver can rebuild the chat from any of them.
	ChatID       string   `json:"chat_id,omitempty"`
	Participants []string `json:"participants,omitempty"`
	// Responders are the participants asked to answer; empty means the
	// message only informs. In a chat ReplyTo is only a reference.
	Responders []string `json:"responders,omitempty"`
	// RootID is the external request an automatic chain of requests started
	// from, AutoDepth how many automatic hops away from it m is.
	RootID    string `json:"root_id,omitempty"`
	AutoDepth uint8  `json:"auto_depth,omitempty"`
	// ActivityInfo details Activity on status updates of chat jobs.
	ActivityInfo *ActivityState `json:"activity_info,omitempty"`
	// HoldReason is why a chat request is not answered automatically, on a
	// JobHeld status (and on the completed status of an answered job); Activity
	// then carries HoldText. Older peers neither send nor read it.
	HoldReason string `json:"hold_reason,omitempty"`
	// ChatGen is the generation of a keyed chat (see KeyedChatID), on every
	// envelope of it; 0 is omitted. Older peers ignore it.
	ChatGen uint32 `json:"chat_gen,omitempty"`
	// AuthorKind says who wrote the message (Author*); empty from older peers.
	AuthorKind string `json:"author_kind,omitempty"`
	// ParticipantIDs are the node ids of Participants (same order), on every
	// envelope of a project chat.
	ParticipantIDs []string `json:"participant_ids,omitempty"`
	// ChatMode is the chat's Mode (ChatModeProject for a standalone project
	// chat), on every envelope of it.
	ChatMode string `json:"chat_mode,omitempty"`
	// ChatOwner and ChatRev are a standalone project chat's Owner and Rev, on
	// every envelope of it; older peers send neither.
	ChatOwner string `json:"chat_owner,omitempty"`
	ChatRev   uint32 `json:"chat_rev,omitempty"`
	// Receipts are the read receipts of a KindReceipt message.
	Receipts []Receipt `json:"receipts,omitempty"`
}

// IsRequest reports whether m is a request outside chats: neither a reply, a
// status update nor a chat message (those ask by Responders, see Asks).
func (m Message) IsRequest() bool { return m.ReplyTo == "" && m.Kind == "" && m.ChatID == "" }

// Asks reports whether m is a chat message that asks name to answer.
func (m Message) Asks(name string) bool {
	return m.ChatID != "" && m.Kind == "" && slices.Contains(m.Responders, name)
}

// Held reports whether m asks for answers but is too many automatic hops
// from its external request: it is kept, and no handler runs for it.
func (m Message) Held() bool { return len(m.Responders) > 0 && m.AutoDepth > MaxAutoDepth }

// MaxAutoDepth is how many agent hops (an agent or the worker writing after
// another node's message, see inheritChain) a chain may take from the last
// message a person wrote; past it a request is held: «пауза — нужен человек».
const MaxAutoDepth = 8

// Entry is a message as listed by the inbox API. Status updates are not
// listed; an outbound request instead carries the latest job status reported
// for it (JobStatus), what the handler is doing while it runs (Activity) and
// the reply body (Answer). An inbound request this node's own handler has not
// answered yet carries that job's status and activity the same way.
type Entry struct {
	Direction string `json:"direction"` // "in" or "out"
	Status    string `json:"status"`    // in: pending|delivered; out: queued|sent
	Peer      string `json:"peer,omitempty"`
	Answer    string `json:"answer,omitempty"`
	// LastHeard is when this node last received a status update or reply for
	// an outbound request, or the request's own time when nothing came yet.
	LastHeard time.Time `json:"last_heard,omitzero"`
	// NoNewsMin is set on an unanswered outbound request the peer has been
	// silent about for NoNewsAfter or longer: minutes since LastHeard.
	NoNewsMin int `json:"no_news_min,omitempty"`
	// Project names the entry's context where an app runs several (a project
	// id, or "legacy"); a node leaves it empty.
	Project string `json:"project,omitempty"`
	Message
}

// DerivedID returns a stable message id for (base, label), so a message that
// is re-sent after a crash keeps its id and the receiver drops the duplicate.
func DerivedID(base, label string) string {
	sum := sha256.Sum256([]byte(base + "/" + label))
	return hex.EncodeToString(sum[:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

func newID() string { return randomHex(16) }

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
