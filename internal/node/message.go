package node

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// AreaPrefix marks an area address in Message.To, e.g. "area:dev".
const AreaPrefix = "area:"

// Message is one agent-to-agent message.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Area      string    `json:"area,omitempty"`
	Body      string    `json:"body"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Entry is a message as listed by the inbox API.
type Entry struct {
	Direction string `json:"direction"` // "in" or "out"
	Status    string `json:"status"`    // in: pending|delivered; out: queued|sent
	Peer      string `json:"peer,omitempty"`
	Message
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
