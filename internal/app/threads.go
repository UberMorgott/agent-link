package app

import (
	"slices"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// Thread is one question together with the answer that belongs to it, so the
// inbox page shows a pair instead of two loose rows. Direction is the
// question's direction: "out" is a question this computer asked, "in" one the
// peer asked.
type Thread struct {
	ID        string     `json:"id"`
	Direction string     `json:"direction"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	Area      string     `json:"area,omitempty"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"created_at"`
	Status    string     `json:"status"`
	Answer    string     `json:"answer,omitempty"`
	AnswerAt  *time.Time `json:"answer_at,omitempty"`
	Answered  bool       `json:"answered"`
	Replyable bool       `json:"replyable"`
	// Activity is what the peer's agent is doing on this question right now.
	Activity string `json:"activity,omitempty"`
	// NoNewsMin: minutes the peer has been silent about this unanswered question.
	NoNewsMin int `json:"no_news_min,omitempty"`
}

// Status values that are not a job status; the job statuses come from node.
const (
	statusAnswered = "answered"
)

// threads pairs inbox entries by reply_to. Every entry that is not a reply
// becomes a thread; a reply is folded into the thread of the message it
// answers, the newest reply winning. A reply whose request is no longer listed
// keeps its own thread. Status updates are ignored: an outbound request
// already carries their latest job status.
func threads(entries []node.Entry) []Thread {
	out := make([]Thread, 0, len(entries))
	index := map[string]int{} // request id -> position in out
	var replies []node.Entry
	for _, e := range entries {
		if e.Kind == node.KindStatus {
			continue
		}
		if e.ReplyTo != "" {
			replies = append(replies, e)
			continue
		}
		index[e.ID] = len(out)
		out = append(out, newThread(e))
	}
	for _, e := range replies {
		i, ok := index[e.ReplyTo]
		if !ok { // the request is not in this page of the inbox
			out = append(out, newThread(e))
			continue
		}
		if t := &out[i]; t.AnswerAt == nil || e.CreatedAt.After(*t.AnswerAt) {
			at := e.CreatedAt
			t.Answer, t.AnswerAt, t.Answered, t.Replyable = e.Body, &at, true, false
			t.Activity, t.NoNewsMin = "", 0
			t.Status = statusAnswered
			if e.JobStatus != "" {
				t.Status = e.JobStatus
			}
		}
	}
	slices.SortFunc(out, func(a, b Thread) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

func newThread(e node.Entry) Thread {
	t := Thread{
		ID: e.ID, Direction: e.Direction, From: e.From, To: e.To, Area: e.Area,
		Body: e.Body, CreatedAt: e.CreatedAt, Status: e.Status,
		Replyable: e.Direction == "in" && e.IsRequest(),
	}
	// Outbound messages address the peer that receives them; an area message
	// carries the area separately.
	if e.Direction == "out" && e.Peer != "" {
		t.To = e.Peer
	}
	if e.JobStatus != "" {
		t.Status = e.JobStatus
	}
	if e.Direction == "out" && e.Answer == "" {
		t.NoNewsMin = e.NoNewsMin
		if e.JobStatus == node.JobRunning {
			t.Activity = e.Activity
		}
	}
	// An outbound request whose reply the store already folded in.
	if e.Answer != "" {
		t.Answer, t.Answered = e.Answer, true
		if e.JobStatus == "" {
			t.Status = statusAnswered
		}
	}
	return t
}
