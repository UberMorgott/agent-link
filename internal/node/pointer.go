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
//     `unread`, or `agentlink chat unread --session <id>` (UnreadQuery with
//     agent=1 and its session). For a session that asked for WakeQueue that
//     call is the delivery (pointerFetch): what it lists is claimed by the
//     rules of a hook's delivery (Claim) and acknowledged at once (Ack), as a
//     hook does with what it injects. So it returns what is unread for the
//     session at that moment, a message that came after the prompt was queued
//     too, and never one that was acknowledged or taken meanwhile; a pointer
//     that runs when nothing waits lists nothing and changes nothing.
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

// PointerPrompt is the prompt that wakes an idle WakeQueue session for total
// messages: no message itself, only where to get them (the MCP tool, else the
// CLI exe with the session's id and folder) and the wake's token (WakeMarker).
func PointerPrompt(total int, folder, session, token, exe string) string {
	cmd := "agentlink"
	if exe != "" {
		cmd = `"` + exe + `"`
	}
	cmd += " chat unread --session " + session
	if folder != "" {
		cmd += ` --folder "` + folder + `"`
	}
	return fmt.Sprintf("agent-link: для этой сессии есть новые сообщения (%d); их текста здесь нет. %s\n"+
		"Получите их сейчас: вызовите инструмент unread MCP-сервера agentlink (без аргументов). "+
		"Если такого инструмента нет, выполните команду (в PowerShell начните её с &): %s\n"+
		"Вызов выдаёт сообщения, которые ждут эту сессию сейчас, и сразу отмечает их прочитанными: прочитайте их и, где просят ответа, ответьте. "+
		"Если он ничего не вернул и сообщений agent-link нет в контексте этого хода, делать нечего: ответьте одной короткой строкой и завершите ход.",
		total, WakeMarker(token), cmd)
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

// pointerFetch delivers page, the unread listing a WakeQueue session asked for
// itself, to that session: the messages the node grants it (Claim, as for its
// hooks) are acknowledged and stay listed, the others are left out. The call
// also proves the session takes its wakes (wakeProven).
func (n *Node) pointerFetch(session string, page UnreadPage) (UnreadPage, error) {
	if len(page.Messages) > 0 {
		granted, err := n.Claim(ClaimRequest{IDs: ids(page.Messages), SessionID: session})
		if err != nil {
			return UnreadPage{}, err
		}
		listed := len(page.Messages)
		page.Messages = slices.DeleteFunc(page.Messages, func(m UnreadMessage) bool { return !slices.Contains(granted, m.ID) })
		page.Total -= listed - len(page.Messages)
		if len(granted) > 0 {
			if _, err := n.Ack("", AckRequest{IDs: granted, SessionID: session}); err != nil {
				return UnreadPage{}, err // still claimed by it: the next call lists them again
			}
			page.Delivered = true
			n.log.Info("session fetched its messages", "session", session, "messages", len(granted))
		}
	}
	n.wakeProven(session)
	return page, nil
}

// pointerShown starts pointerFetchGrace on the leases of ids session holds by
// the pointer wake token: its rollout showed the pointer's prompt.
func (n *Node) pointerShown(session, token string, ids []string) {
	changed, err := n.leases.shown(session, token, ids, time.Now())
	if err != nil {
		n.log.Warn("save leases", "err", err)
	}
	if changed {
		n.changed("leases")
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
