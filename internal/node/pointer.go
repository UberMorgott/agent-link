package node

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Pointer wakes. `codex queue` keeps every prompt it accepted and nothing can
// look into it or take a prompt back, so a queued prompt may run minutes or
// hours after its messages were read, acknowledged and answered some other
// way. The prompt the node queues in a WakeQueue session therefore carries no
// message: it is a pointer (PointerPrompt), the wake's marker and the one
// instruction to fetch what waits for the session now.
//
//   - The fetch is the session's own unread listing: the agentlink MCP tool
//     `unread` (called with the session's id, which the prompt names), or
//     `agentlink chat unread --session <id>` (UnreadQuery with agent=1 and its
//     session). For a session that asked for WakeQueue that call is the
//     delivery (pointerFetch): what it answers with is claimed by the rules of
//     a hook's delivery (Claim) and acknowledged at once (Ack), as a hook does
//     with what it injects. So it returns what is unread for the session at
//     that moment, a message that came after the prompt was queued too, and
//     never one that was acknowledged or taken meanwhile; a pointer that runs
//     when nothing waits lists nothing and changes nothing.
//   - One answer carries what a hook's batch does (FetchCut), and only what it
//     carries is acknowledged: the rest stays unread, and the answer says so
//     (UnreadPage.Note). A fetch has no cursor: what it delivered is read and
//     listed no more, so the call for the rest is the same call, which lists
//     from the start (`after` is ignored, Next never set; a cursor would pass
//     over a message listed before it and not delivered, issue #41). The
//     control API, which merges the
//     listings of several contexts, cuts first and delivers what it returns,
//     each message in its own context (UnreadListing, Deliver, FetchDone).
//   - One pointer at a time (pointerPending): while one is in the session's
//     queue and its rollout has not shown it, the node queues no other. What a
//     wake is due for meanwhile is leased to the session under that pointer's
//     token (wakeIdle), with the schedule, count and bounds of a wake queued
//     anew (staleWakeWait, maxStaleWakes, queued_stale, needs_human).
//   - A pointer is not waited for for ever. It counts as lost once a lease of
//     it failed unproven (queued_stale: the session silent for queuedOwnerMax
//     with it queued), or once the session's hooks reported a turn after it
//     was queued, that turn's end, and pointerLostWait later the rollout still
//     has no marker of it (reasonLost: its leases go back to retry; not while
//     the rollout shows a turn running), or once a lease of it stayed in retry
//     with the pointer still queued ("stopped", "session_gone") for
//     queuedOwnerMax (orphanPointers). A lost
//     pointer keeps no new one out of the queue: the next wake that is due
//     queues one, under a new token, on the schedule and caps of every wake,
//     and its fetch delivers what is unread then, the lost pointer's messages
//     too. So no message is named by two unconsumed pointers, and a lost
//     pointer that runs after all fetches what waits then or nothing.
//   - The pointer's prompt in the rollout (observeQueueStart) proves the
//     pointer was shown, not that a message was read: it ends the wake count
//     (wakeProven) and starts pointerFetchGrace on the wake's leases
//     (Lease.Shown), which stay the session's meanwhile. No fetch within it:
//     they are revoked (reasonUnfetched: retry, their backoff and attempts as
//     ever, needs_human at maxLeaseAttempts), their authors hear
//     held:wake_unfetched, and a hook, another session or a new pointer takes
//     them.
//   - A pointer holds nothing back from the session's hooks: they deliver its
//     messages at their next event as any other, and the pointer then fetches
//     what is left.
//
// A pointer's wake token starts with pointerTokenPrefix; a lease whose token
// does not is a prompt of a node before pointers, which carried the messages:
// its marker still acknowledges them (observeQueueStart).

// pointerTokenPrefix starts the wake token of a pointer wake; randomHex, the
// token of every other wake, has no '-'.
const pointerTokenPrefix = "p-"

// pointerToken reports whether wake token t is a pointer wake's.
func pointerToken(t string) bool { return strings.HasPrefix(t, pointerTokenPrefix) }

// reasonUnfetched is the lease reason of a pointer wake whose turn ran and
// did not fetch the message within pointerFetchGrace.
const reasonUnfetched = "pointer_unfetched"

// reasonLost is the lease reason of a pointer wake the session's thread never
// ran: it took a turn after the pointer was queued and its rollout shows no
// marker of it (pointerTurned, pointerLostWait).
const reasonLost = "pointer_lost"

// PointerPrompt is the prompt that wakes an idle WakeQueue session: no
// message itself and no number of them (the fetch answers with what waits for
// the session when it runs, which is not what the wake was queued for), only
// where to get them (the MCP tool, called with the session's id, as a client's
// call may not name its thread; else the CLI exe with that id) and the wake's
// token (WakeMarker). An MCP server of an older install, still running beside
// this node, knows no session argument and refuses a call with it (its schema
// allows no other property): the prompt says to call again without arguments
// then (that server's call names the thread by its _meta), and to run the
// command when an answer shows messages it did not deliver. The command names
// no folder: the node takes the session's own (UnreadListing), and a path in
// a prompt would be text the agent's shell expands.
func PointerPrompt(session, token, exe string) string {
	cmd := "agentlink"
	if exe != "" {
		cmd = `"` + exe + `"`
	}
	cmd += " chat unread --session " + session
	return fmt.Sprintf("agent-link: для этой сессии есть новые сообщения; их текста здесь нет. %s\n"+
		"Получите их сейчас: вызовите инструмент unread MCP-сервера agentlink с одним аргументом session: \"%s\". "+
		"Если вызов отклонён из-за этого аргумента (инструмент его не принимает), вызовите unread ещё раз без аргументов. "+
		"Если такого инструмента нет или в его ответе есть сообщения, но нет delivered: true, выполните команду (в PowerShell начните её с &): %s\n"+
		"Вызов выдаёт сообщения, которые ждут эту сессию сейчас, и сразу отмечает их прочитанными: прочитайте их и, где просят ответа, ответьте. "+
		"Если в ответе есть note, выдана только часть: повторяйте тот же вызов с теми же аргументами (без after), пока note не исчезнет. "+
		"Если он ничего не вернул и сообщений agent-link нет в контексте этого хода, делать нечего: ответьте одной короткой строкой и завершите ход.",
		WakeMarker(token), session, cmd)
}

// fetchSession is the live session whose own unread listing is its delivery:
// one that asked for WakeQueue (the node wakes it by pointers).
func (n *Node) fetchSession(session string) (Session, bool) {
	if session == "" {
		return Session{}, false
	}
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[session]
	if s == nil || !s.live(now) || s.Asked != WakeQueue {
		return Session{}, false
	}
	return *s, true
}

// FetchFolder is the folder of session when its own unread listing is its
// delivery here (fetchSession): the control API asks for that folder's
// messages when the call names none.
func (n *Node) FetchFolder(session string) (string, bool) {
	s, ok := n.fetchSession(session)
	return s.Folder, ok
}

// pointerFetch delivers page, the unread listing a WakeQueue session asked for
// itself, to that session: as much of it as one answer carries (FetchCut), of
// which the messages the node grants it are acknowledged and stay listed
// (Deliver, FetchDone).
func (n *Node) pointerFetch(session string, page UnreadPage) (UnreadPage, error) {
	more := FetchCut(&page)
	granted, err := n.Deliver(session, ids(page.Messages))
	if err != nil {
		return UnreadPage{}, err
	}
	FetchDone(&page, granted, more)
	return page, nil
}

// FetchCut cuts a fetch's listing to what one answer carries: what a hook's
// batch does (FitUnread, FormatBudget), so one tool result is never a whole
// page of full bodies, and nothing is acknowledged that the answer does not
// show. It reports whether more wait behind it (a next page, or the cut).
func FetchCut(page *UnreadPage) bool {
	more := page.Next != ""
	if fit := FitUnread(page.Messages); fit < len(page.Messages) {
		page.Messages, more = page.Messages[:fit], true
	}
	return more
}

// Deliver is the delivery of a fetch's messages ids to session: the ones the
// node grants it (Claim, as for its hooks; to a session of another context of
// this process the replies to its own messages) are acknowledged at once
// (Ack), as a hook does with what it injects, and returned. The call also
// proves the session takes its wakes (wakeProven).
func (n *Node) Deliver(session string, ids []string) ([]string, error) {
	var granted []string
	if len(ids) > 0 {
		var err error
		if granted, err = n.Claim(ClaimRequest{IDs: ids, SessionID: session}); err != nil {
			return nil, err
		}
		if len(granted) > 0 {
			if _, err := n.Ack("", AckRequest{IDs: granted, SessionID: session}); err != nil {
				return nil, err // still claimed by it: the next call lists them again
			}
			n.log.Info("session fetched its messages", "session", session, "messages", len(granted))
		}
	}
	n.wakeProven(session)
	return granted, nil
}

// fetchMore is UnreadPage.Note of a fetch that left messages behind; with
// their number when it is known. FetchPartNote is the one of a fetch a part
// of which could not be delivered (a context's Deliver failed).
const (
	FetchPartNote  = "Это не все сообщения: часть не удалось выдать (ошибка узла), они не отмечены прочитанными. Повторите этот же вызов: он выдаст их или сообщит об ошибке."
	fetchMore      = "Это не все сообщения: остальные ждут эту сессию и не отмечены прочитанными. Повторите этот же вызов, чтобы получить следующие."
	fetchMoreCount = "Это не все сообщения: ещё %d ждут эту сессию и не отмечены прочитанными. Повторите этот же вызов, чтобы получить следующие."
)

// FetchDone makes page, the listing a fetch asked Deliver for, its answer:
// the messages granted (read now, Delivered), the others left out, and, when
// more wait behind them, the plain word to call again (Note). Never Next: the
// call for the rest is the same call (a fetch's listing starts anew, the
// delivered messages are in it no more), and a cursor of this listing would
// pass over a message it listed and did not deliver.
func FetchDone(page *UnreadPage, granted []string, more bool) {
	listed := len(page.Messages)
	page.Messages = slices.DeleteFunc(page.Messages, func(m UnreadMessage) bool { return !slices.Contains(granted, m.ID) })
	page.Total -= listed - len(page.Messages)
	page.Delivered = len(page.Messages) > 0
	page.Next, page.Note = "", ""
	if more && page.Delivered {
		// An answer with nothing in it asks for no further call: what is left
		// is not this session's to take.
		page.Note = fetchMore
		if left := page.Total - len(page.Messages); left > 0 {
			page.Note = fmt.Sprintf(fetchMoreCount, left)
		}
	}
}

// pointerShown records that session's rollout showed the prompt of the
// pointer wake token: fetch grace starts on its leases (leaseBook.shown).
func (n *Node) pointerShown(session, token string) {
	changed, err := n.leases.shown(session, token, time.Now())
	if err != nil {
		n.log.Warn("save leases", "err", err)
	}
	if changed {
		n.changed("leases")
	}
}

// pointerPending is the wake token of the pointer queued for s that its
// rollout has not shown yet and that is not lost ("" when none): while there
// is one, no other is queued for it (owner decision, issue #40) and what comes
// meanwhile is leased to it under that token (wakeIdle), as that pointer's
// turn fetches everything that waits then. A pointer the rollout shows
// meanwhile, though no observer of it ran, is recorded shown here.
//
// settling: the session's hooks reported a turn after the pointer was queued
// and its end less than pointerLostWait ago (pointerTurned): the thread may
// run the pointer next, so nothing is queued or leased for it now. Past that
// wait the pointer is lost (pointerLost) and none is pending.
func (n *Node) pointerPending(s Session, now time.Time) (token string, settling bool) {
	token, at := n.leases.pendingPointer(s.SessionID)
	if token == "" {
		return "", false
	}
	if path := codexRollout(codexHomeOf(s), s.SessionID); path != "" && rolloutContains(path, WakeMarker(token)) {
		n.pointerShown(s.SessionID, token)
		return "", false
	}
	if !pointerTurned(s, at) {
		return token, false
	}
	if now.Sub(s.IdleAt) < pointerLostWait {
		return token, true
	}
	n.pointerLost(s.SessionID, token)
	return "", false
}

// pointerTurned reports whether idle session s took a turn its hooks reported
// after a pointer was queued for it at at, and ended it: a pointer its
// rollout does not show by then was not run by that turn nor after it.
func pointerTurned(s Session, at time.Time) bool {
	return s.Idle && s.TurnAt.After(at) && !s.IdleAt.Before(s.TurnAt)
}

// pointerLost ends the wait for session's pointer of the wake token, which
// its thread never ran (reasonLost): its messages are free for a new pointer,
// a hook or another session, with their attempts and backoff as ever.
func (n *Node) pointerLost(session, token string) {
	released, failed, err := n.leases.lost(session, token, time.Now())
	if err != nil {
		n.log.Warn("save leases", "err", err)
	}
	if len(released) == 0 {
		return
	}
	n.dropClaims(session, released)
	n.log.Info("a queued pointer was never run; its messages wait for a new one", "session", session, "messages", len(released))
	if len(failed) > 0 {
		n.log.Warn("delivery failed after every lease; a person decides", "owner", session, "messages", len(failed))
		n.report(failed, AttemptNeedsHuman)
	}
	n.changed("leases")
}

// lostPointers ends the wait for every pointer its idle session did not run
// in a turn it took since (pointerPending), also when no newer message would
// make wakeIdle look: the lost pointer's own messages may get a new one.
//
// A thread whose rollout shows a turn running that its hooks did not report
// has not passed its pointer over: the pointer is late behind that turn and
// runs after it (queueHeld holds the wakes meanwhile), so it is not lost now.
func (n *Node) lostPointers(now time.Time) {
	r := n.sess
	var idle []Session
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.Idle && s.Asked == WakeQueue && s.live(now) && !s.TurnAt.IsZero() && !s.IdleAt.Before(s.TurnAt) && now.Sub(s.IdleAt) >= pointerLostWait {
			idle = append(idle, *s)
		}
	}
	r.mu.Unlock()
	for _, s := range idle {
		if _, at := n.leases.pendingPointer(s.SessionID); pointerTurned(s, at) {
			if _, busy := n.turnRunning(s, now); busy {
				continue
			}
			n.pointerPending(s, now)
		}
	}
}

// orphanPointers ends the wait for every pointer whose lease went back to
// retry with the pointer still in its session's queue (the emergency stop, a
// registration that lapsed) and stayed so for queuedOwnerMax
// (leaseBook.orphaned): no hook event, turn or deadline would end it, the
// message would be handed for ever and never woken for again, and what came
// later would be leased under a pointer nothing waits for. It is lost as any
// other (pointerLost): a new pointer may be queued for its messages. Not while
// the session's rollout shows a turn running (the pointer runs after it).
func (n *Node) orphanPointers(now time.Time) {
	for _, p := range n.leases.orphaned(now) {
		n.sess.mu.Lock()
		s := n.sess.sessions[p.owner]
		var cur Session
		if s != nil {
			cur = *s
		}
		n.sess.mu.Unlock()
		if s != nil && cur.live(now) {
			if _, busy := n.turnRunning(cur, now); busy {
				continue
			}
		}
		n.pointerLost(p.owner, p.token)
	}
}

// unfailed are the ids whose lease did not fail.
func (n *Node) unfailed(ids []string) []string {
	var out []string
	for _, id := range ids {
		if l, ok := n.leases.get(id); !ok || !l.Failed {
			out = append(out, id)
		}
	}
	return out
}
