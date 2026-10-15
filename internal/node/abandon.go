package node

// Abandoned questions. A discuss caller waits for the asked seat's reply a
// bounded time; when that wait times out the caller has moved on (a subagent
// that asked may have ended). A reply that comes later must not land in
// another session as unasked work: the node records the timeout on the
// question itself (chatRecord.AbandonedAt), atomically against the reply's
// storing, and such a reply stays in the chat's history only. Only a real
// timeout abandons: a dropped connection or a cancelled call does not, and an
// async, queued or held ask is never waited for, so its reply is delivered.

// AbandonAsk records that the direct wait of the asker of question id (in
// chat) for seat's reply timed out. When seat replied already, nothing is
// recorded and answered is true: the caller hands that reply over instead.
// Otherwise every later reply of a seat to it is kept in history only: unread
// for no session (no hook, wake, Codex native wake or orphan hand-over) and
// pushed to no seat. The mark is saved with the question, so it survives a
// restart.
//
// The mark is per question, not per seat: a discuss asks one seat, so any
// seat's reply to it counts.
func (n *Node) AbandonAsk(chat, id, seat string) (answered bool, err error) {
	n.askMu.Lock() // not between a peer reply's seat hand-over and its storing (receive)
	defer n.askMu.Unlock()
	return n.chats.abandon(chat, id, seat)
}

// ResumeAsk drops question id's abandon mark (AbandonAsk): its asker waits
// for the reply again, and a reply from now on is delivered as usual.
func (n *Node) ResumeAsk(id string) error {
	return n.chats.resume(id)
}
