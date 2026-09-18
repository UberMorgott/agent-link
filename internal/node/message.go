package node

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// AreaPrefix marks an area address in Message.To, e.g. "area:dev".
const AreaPrefix = "area:"

// KindStatus marks a job status update: a bodiless message whose ReplyTo is
// the request and whose JobStatus is queued or running. Status updates never
// wake wait and never start a handler.
const KindStatus = "status"

// Job statuses carried in Message.JobStatus.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
)

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
}

// IsRequest reports whether m asks for an answer: it is neither a reply nor a status update.
func (m Message) IsRequest() bool { return m.ReplyTo == "" && m.Kind == "" }

// Entry is a message as listed by the inbox API. Status updates are not
// listed; an outbound request instead carries the latest job status reported
// for it (JobStatus), what the handler is doing while it runs (Activity) and
// the reply body (Answer).
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
