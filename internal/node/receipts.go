package node

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Read state of messages for this node's sessions: every new message from
// another node, and every message a person here wrote (own_human, as
// information), is unread until a session acknowledges it (Ack) or the worker
// takes it (ClaimRun). A browser showing it changes nothing. An acknowledged
// message of another node sends that node a read receipt (KindReceipt),
// queued in the outbox like any message, so an offline author gets it when it
// connects; a reply tells its author it was answered. The author shows the
// state per recipient (Delivery.State).

// ErrFolderUnbound: the folder is neither the working folder nor a project
// folder of this node.
var ErrFolderUnbound = errors.New("folder is not the working folder or a project folder of this node")

// ErrNeedsFolder: a project node has no folder bound, so no folder is its
// (docs/plans/projects-v1.md §3.5): no agent session can register there.
var ErrNeedsFolder = errors.New("no project folder is bound")

// folderMap maps local folders to areas: the deepest project folder that
// holds a folder picks its area; the working folder (any folder when none is
// set) is the one of direct messages and chats without a project here (area
// ""). Without SetFolders every folder is the working folder.
type folderMap struct {
	work     string
	projects map[string]string // area -> dir
}

// NeedsFolder reports a project node without a bound folder: it has no
// folder at all (FolderArea matches nothing), and requests that ask it are
// held (HoldWithoutFolder). Outside projects an empty working folder means any.
func (n *Node) NeedsFolder() bool { return n.cfg.Project != "" && n.folders.work == "" }

// SetFolders tells the node its working folder and project folders (area ->
// dir), which bind a session's folder to an area (Sessions, Unread). It must
// be set before Serve or Run.
func (n *Node) SetFolders(workDir string, projects map[string]string) {
	n.folders = folderMap{work: workDir, projects: projects}
}

// FolderArea returns the area a session in folder works on; ok is false when
// folder is no folder of this node (ErrFolderUnbound).
func (n *Node) FolderArea(folder string) (area string, ok bool) {
	if n.NeedsFolder() {
		return "", false
	}
	best := -1
	for a, dir := range n.folders.projects {
		if dir != "" && inFolder(dir, folder) && len(filepath.Clean(dir)) > best {
			area, best = a, len(filepath.Clean(dir))
		}
	}
	if best >= 0 {
		return area, true
	}
	if n.folders.work == "" || inFolder(n.folders.work, folder) {
		return "", true
	}
	return "", false
}

// localArea is the area whose session gets a message of area: that area when
// it has a project folder here, else the working folder's ("").
func (n *Node) localArea(area string) string {
	if n.folders.projects[area] != "" {
		return area
	}
	return ""
}

// inFolder reports whether path is dir or inside it (case-insensitively on
// Windows, like filepath.Rel).
func inFolder(dir, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// UnreadMessage is one unread message for this node's sessions.
type UnreadMessage struct {
	ChatMessage
	// Cursor orders unread messages; pass the last one as after for the next page.
	Cursor     string    `json:"cursor"`
	ReceivedAt time.Time `json:"received_at"`
	// AsksYou: the message asks this node to answer (a chat request with this
	// node among its responders, or a plain request). OwnHuman ones never do.
	AsksYou bool `json:"asks_you,omitempty"`
	// Paused: past MaxAutoDepth; shown as information on the next human turn.
	Paused bool `json:"paused,omitempty"`
	// WakeToken (UnreadPage.Woken only): the token of the wake prompt that
	// carried the message (WakeMarker); only a prompt with it acknowledges it.
	WakeToken string `json:"wake_token,omitempty"`
	// ForSeat: the seat (seats.go) the message is for; it goes to that seat's
	// session only.
	ForSeat string `json:"for_seat,omitempty"`
}

// UnreadPage is one page of Unread.
type UnreadPage struct {
	Messages []UnreadMessage `json:"messages"`
	Total    int             `json:"total"`          // unread messages matching, all pages
	Next     string          `json:"next,omitempty"` // cursor of the next page, if any
	// Woken (for a session only): its unread messages the prompt that woke it
	// carried (wakeIdle), kept out of Messages and Total: its hook acknowledges
	// them at that prompt instead of delivering them again.
	Woken []UnreadMessage `json:"woken,omitempty"`
}

// Unread lists this node's unread messages, oldest first, of every age:
// limit (0: 50) after cursor after. A non-empty folder keeps only the
// messages for a session there (FolderArea, localArea); ErrFolderUnbound for
// a folder that is none of this node's.
func (n *Node) Unread(folder, after string, limit int) (UnreadPage, error) {
	return n.UnreadFor(folder, "", after, limit)
}

// UnreadFor is Unread for one live session: a non-empty session keeps only
// the messages it may take (routeOf: the ones for it and the ones for no
// session in particular), never those another live session is to get.
func (n *Node) UnreadFor(folder, session, after string, limit int) (UnreadPage, error) {
	return n.unreadFor(folder, session, after, limit, false)
}

// unreadFor optionally excludes guarded messages from automatic wake and Stop.
func (n *Node) unreadFor(folder, session, after string, limit int, actionable bool) (UnreadPage, error) {
	if limit <= 0 {
		limit = 50
	}
	area, filter := "", folder != ""
	if filter {
		a, ok := n.FolderArea(folder)
		if !ok && n.NeedsFolder() {
			return UnreadPage{}, ErrNeedsFolder
		}
		if !ok {
			return UnreadPage{}, fmt.Errorf("%w: %s", ErrFolderUnbound, folder)
		}
		area = a
	}
	var live map[string]bool
	if session != "" {
		n.sess.claimMu.Lock()
		defer n.sess.claimMu.Unlock()
		live = n.sess.liveIDs(time.Now())
	}
	mine := func(id string, rec *chatRecord) bool {
		if session == "" {
			return true
		}
		to := n.routeOf(id, rec, live)
		return to == "" || to == session
	}
	var all, woken []UnreadMessage
	wake := func(um *UnreadMessage) bool {
		if session == "" {
			return false
		}
		token, ok := n.wokeWith(um.ID, session, live)
		um.WakeToken = token
		return ok
	}
	for _, u := range n.chats.unread() {
		if filter && n.localArea(u.chat.Area) != area {
			continue
		}
		if !mine(u.rec.Message.ID, &u.rec) {
			continue
		}
		um := UnreadMessage{ChatMessage: n.chatMessage(u.chat, u.rec), ReceivedAt: u.rec.ReceivedAt}
		um.AsksYou = !um.OwnHuman && um.Asks(n.cfg.Node)
		um.Paused = um.AsksYou && um.Message.Held()
		if actionable && um.Paused {
			continue
		}
		n.materialize(&um.Message, u.chat.Area)
		if wake(&um) {
			woken = append(woken, um)
			continue
		}
		all = append(all, um)
	}
	for _, r := range n.store.unreadPlain() {
		if filter && n.localArea(r.Message.Area) != area {
			continue
		}
		if !mine(r.Message.ID, nil) {
			continue
		}
		um := UnreadMessage{Direction: "in", Unread: true, Message: r.Message,
			ReceivedAt: r.ReceivedAt, AsksYou: r.Message.IsRequest()}
		n.materialize(&um.Message, r.Message.Area)
		if wake(&um) {
			woken = append(woken, um)
			continue
		}
		all = append(all, um)
	}
	if session != "" {
		seen := map[string]bool{}
		for _, m := range all {
			seen[m.ID] = true
		}
		msgs, w := n.seatUnread(session, filter, area, actionable)
		for _, m := range msgs {
			if !seen[m.ID] {
				all = append(all, m)
			}
		}
		woken = append(woken, w...)
	}
	for i := range all {
		all[i].Cursor = fmt.Sprintf("%020d-%s", all[i].ReceivedAt.UnixNano(), all[i].ID)
	}
	slices.SortFunc(all, func(a, b UnreadMessage) int { return strings.Compare(a.Cursor, b.Cursor) })
	page := UnreadPage{Messages: []UnreadMessage{}, Total: len(all), Woken: woken}
	for _, m := range all {
		if after != "" && m.Cursor <= after {
			continue
		}
		if len(page.Messages) == limit {
			page.Next = page.Messages[limit-1].Cursor
			break
		}
		page.Messages = append(page.Messages, m)
	}
	return page, nil
}

// AckRequest is the body of POST /chats/{id}/ack and POST /ack.
type AckRequest struct {
	IDs       []string `json:"ids"`
	SessionID string   `json:"session_id,omitempty"` // the reading session, if registered
}

// AckResult is the outcome of acknowledging one message.
type AckResult struct {
	ID    string `json:"id"`
	Found bool   `json:"found"`
	// WasUnread: this ack read it; false for one read before.
	WasUnread bool `json:"was_unread,omitempty"`
	// Assigned is who answers it on this node when it asks this node: the
	// acking session ("session:<id>", "session" without an id), or "worker"
	// when the worker took it first (the session must not answer it too).
	Assigned string `json:"assigned,omitempty"`
}

// Ack acknowledges messages for this node: they are read from now on. chat,
// when not empty, restricts the ids to that chat's messages (others are not
// found); an empty chat takes chat messages and plain ones. A request that
// asks this node and nobody took yet is assigned to the acking session, so the
// worker leaves it. Each newly read message of another node sends its author
// a read receipt. Acking again changes nothing.
func (n *Node) Ack(chat string, req AckRequest) ([]AckResult, error) {
	if len(req.IDs) == 0 {
		return nil, fmt.Errorf("%w: ids required", ErrBadRequest)
	}
	if len(req.IDs) > 1000 {
		return nil, fmt.Errorf("%w: at most 1000 ids", ErrBadRequest)
	}
	if chat != "" {
		if _, ok := n.chats.get(chat); !ok {
			return nil, fmt.Errorf("%w %s", ErrUnknownChat, chat)
		}
	}
	owner := "session"
	if req.SessionID != "" {
		owner += ":" + req.SessionID
	}
	out := make([]AckResult, 0, len(req.IDs))
	receipts := map[string][]string{}
	changed := false
	// A seat's messages are read by its session alone (seats.go).
	seat := n.seatAck(req.SessionID, "", req.IDs)
	for _, id := range req.IDs {
		res := AckResult{ID: id}
		if r, ok := n.chats.message(id); ok && (chat == "" || r.Message.ChatID == chat) && r.Message.Kind == "" {
			rec, wasUnread, err := n.chats.markRead(id, owner, n.cfg.Node)
			if err != nil {
				return out, err
			}
			res.Found, res.WasUnread, res.Assigned = true, wasUnread, rec.Assigned
			if seat[id] {
				res.WasUnread, res.Assigned = true, owner
			}
			if wasUnread && rec.Message.From != n.cfg.Node {
				receipts[rec.Message.From] = append(receipts[rec.Message.From], id)
			}
			changed = changed || wasUnread
		} else if chat == "" {
			found, wasUnread, err := n.store.markRead(id)
			if err != nil {
				return out, err
			}
			res.Found, res.WasUnread = found, wasUnread
			changed = changed || wasUnread
		}
		out = append(out, res)
	}
	n.sess.claimMu.Lock()
	for _, id := range req.IDs {
		delete(n.sess.claims, id) // read now: nobody's to deliver any more
	}
	n.sess.claimMu.Unlock()
	n.sendReceipts(receipts, StateRead)
	var read []string
	for _, res := range out {
		if res.WasUnread {
			read = append(read, res.ID)
		}
	}
	n.confirmRead(read)
	if changed {
		n.changed("messages")
	}
	return out, nil
}

// sendReceipts queues, for every author, one receipt message per chat for
// its messages ids, at state.
func (n *Node) sendReceipts(byAuthor map[string][]string, state string) {
	now := time.Now().UTC()
	for author, ids := range byAuthor {
		byChat := map[string][]string{}
		for _, id := range ids {
			if r, ok := n.chats.message(id); ok {
				byChat[r.Message.ChatID] = append(byChat[r.Message.ChatID], id)
			}
		}
		for chatID, ids := range byChat {
			c, ok := n.chats.get(chatID)
			i := slices.Index(c.Participants, author)
			if !ok || i < 0 || !n.pinned(c, i) {
				continue
			}
			slices.Sort(ids)
			m := Message{ID: DerivedID(strings.Join(ids, ","), n.cfg.Node+"/receipt/"+state), From: n.cfg.Node, To: author,
				Kind: KindReceipt, CreatedAt: now}
			c.stamp(&m)
			for _, id := range ids {
				m.Receipts = append(m.Receipts, Receipt{ID: id, State: state, At: now})
			}
			if err := n.enqueue(author, m); err != nil {
				n.log.Warn("queue receipt", "peer", author, "chat", chatID, "err", err)
			}
		}
	}
}

// receiveReceipts applies peer's receipts to this node's own messages.
func (n *Node) receiveReceipts(peer string, m Message) {
	changed := false
	for _, rc := range m.Receipts {
		if !validID(rc.ID) || (rc.State != StateRead && rc.State != StateAnswered) {
			continue
		}
		r, ok := n.chats.message(rc.ID)
		if !ok || r.Message.From != n.cfg.Node || r.Message.ChatID != m.ChatID {
			continue
		}
		rc.At = cmp.Or(rc.At, m.CreatedAt)
		ok, err := n.chats.applyReceipt(peer, rc)
		if err != nil {
			n.log.Warn("apply receipt", "peer", peer, "id", rc.ID, "err", err)
		}
		changed = changed || ok
	}
	for i, a := range m.Attempts {
		if i >= maxAttemptsPerMessage {
			break
		}
		if !validID(a.ID) || !validAttemptEvent(a.Event) {
			continue
		}
		r, ok := n.chats.message(a.ID)
		if !ok || r.Message.From != n.cfg.Node || r.Message.ChatID != m.ChatID {
			continue
		}
		a.At = cmp.Or(a.At, m.CreatedAt)
		ok, err := n.chats.applyAttempt(peer, a)
		if err != nil {
			n.log.Warn("apply delivery attempt", "peer", peer, "id", a.ID, "err", err)
		}
		changed = changed || ok
	}
	if changed {
		n.changed("messages")
	}
}

// sendAttempts reports delivery attempt event on the chat messages ids to
// their authors: one KindReceipt message per author and chat carrying
// Attempts (no Receipts). Messages of this node itself and plain ones are
// skipped.
func (n *Node) sendAttempts(ids []string, event string) {
	if len(ids) == 0 || !validAttemptEvent(event) {
		return
	}
	now, total := time.Now().UTC(), len(ids)
	type key struct{ author, chat string }
	groups := map[key][]string{}
	for _, id := range ids {
		r, ok := n.chats.message(id)
		if !ok || r.Message.Kind != "" || r.Message.From == n.cfg.Node {
			continue
		}
		k := key{r.Message.From, r.Message.ChatID}
		groups[k] = append(groups[k], id)
	}
	for k, ids := range groups {
		c, ok := n.chats.get(k.chat)
		i := slices.Index(c.Participants, k.author)
		if !ok || i < 0 || !n.pinned(c, i) {
			continue
		}
		slices.Sort(ids)
		for len(ids) > 0 {
			part := ids[:min(len(ids), maxAttemptsPerMessage)]
			ids = ids[len(part):]
			m := Message{ID: DerivedID(strings.Join(part, ","), fmt.Sprintf("%s/attempt/%s/%d", n.cfg.Node, event, now.UnixNano())),
				From: n.cfg.Node, To: k.author, Kind: KindReceipt, CreatedAt: now}
			c.stamp(&m)
			for _, id := range part {
				m.Attempts = append(m.Attempts, Attempt{ID: id, Event: event, At: now})
			}
			if err := n.enqueue(k.author, m); err != nil {
				n.log.Warn("queue delivery attempt", "peer", k.author, "chat", k.chat, "err", err)
			}
		}
	}
	n.log.Info("delivery attempt", "event", event, "messages", total)
}
