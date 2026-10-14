package node

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/humantime"
)

// fetch is the session's own unread listing, as its MCP tool `unread` and
// `agentlink chat unread --session s-q` ask for it.
func (q *staleQueue) fetch() UnreadPage {
	q.t.Helper()
	page, err := q.a.UnreadQuery(url.Values{"agent": {"1"}, "session": {"s-q"}})
	if err != nil {
		q.t.Fatal(err)
	}
	return page
}

// consumed is the thread running the queued pointer of token: its prompt is in
// the rollout at path, and the node's observer of that wake has seen it.
func (q *staleQueue) consumed(path, token string, ids ...string) {
	q.t.Helper()
	q.wrote(path, WakeMarker(token)+"\n")
	q.a.observeQueueStart(q.t.Context(), q.session(), token, ids) // unless the wake's own observer still runs (ids: none = the token's)
	eventually(q.t, "the pointer's observer ended", func() bool { _, on := q.a.observed.Load("s-q\x00" + token); return !on })
}

// pending is the token of the pointer the node still waits for in the thread's
// queue ("" for none).
func (q *staleQueue) pending() string {
	token, _ := q.a.leases.pendingPointer("s-q")
	return token
}

// state is what a pointer that finds nothing must leave as it is: the lease
// book, the session and what is unread.
func (q *staleQueue) state() (map[string]Lease, Session, []string) {
	q.t.Helper()
	page, err := q.a.Unread("", "", 100)
	if err != nil {
		q.t.Fatal(err)
	}
	return q.a.leases.all(), q.session(), ids(page.Messages)
}

// The field case. One pointer is queued for a message; the thread, inside an
// earlier turn, reads it with `unread`; the pointer runs minutes later. The
// message is presented once, by the fetch, which acknowledges it; the pointer's
// turn fetches nothing and changes nothing.
func TestPointerAfterEarlierFetchChangesNothing(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout(turnLine("task_started", time.Now().Add(-time.Minute)))
	q.reg(true) // its hooks said idle since: the node queues
	m := q.send("request read inside an earlier turn")
	q.pass()
	token := wakeTokenOf(q.lastWake())
	if q.w.count() != 1 || !pointerToken(token) || strings.Contains(q.w.texts[0], m.Body) || strings.Contains(q.w.texts[0], m.ID) {
		t.Fatalf("the queued prompt is no pointer: %v", q.w.texts)
	}
	if l, _ := q.a.leases.get(m.ID); l.State != LeaseLeased || l.Via != ViaQueue || l.Attempts != 1 || l.Token != token {
		t.Fatalf("lease of the queued pointer: %+v", l)
	}

	// The earlier turn's `unread`: the one presentation, read by it.
	page := q.fetch()
	if !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != m.ID || page.Messages[0].Body != m.Body {
		t.Fatalf("the fetch inside the earlier turn: %+v", page)
	}
	if l, _ := q.a.leases.get(m.ID); l.State != LeaseAcked {
		t.Fatalf("lease after the fetch: %+v", l)
	}
	if s := q.session(); s.Woken || s.Wakes != 0 {
		t.Fatalf("a session that fetches is still counted woken: %+v", s)
	}
	waitAttempts(t, q.b, m, AttemptWakeRequested, AttemptWokenConfirmed)

	// The pointer runs late: nothing to fetch, nothing changed.
	leases, sess, unread := q.state()
	q.consumed(path, token, m.ID)
	if late := q.fetch(); late.Delivered || len(late.Messages) != 0 || late.Total != 0 || len(late.Woken) != 0 {
		t.Fatalf("the late pointer presented something again: %+v", late)
	}
	l2, s2, u2 := q.state()
	if !reflect.DeepEqual(leases, l2) || !reflect.DeepEqual(sess, s2) || !slices.Equal(unread, u2) {
		t.Fatalf("a pointer with nothing pending changed state:\nleases %+v\n    -> %+v\nsession %+v\n     -> %+v\nunread %v -> %v", leases, l2, sess, s2, unread, u2)
	}
	q.pass()
	if q.w.count() != 1 {
		t.Fatalf("queued again after the late pointer: %v", q.w.calls)
	}
	if got := attemptsOf(q.b, m); !slices.Equal(got, []string{AttemptWakeRequested, AttemptWokenConfirmed}) {
		t.Fatalf("attempts after the late pointer: %v", got)
	}
}

// A pointer consumed late fetches what waits for the session then: a message
// that came after it was queued too, never one read meanwhile, though its own
// lease failed unproven long before (and the newer message got a pointer of
// its own by then, which finds nothing left).
func TestPointerConsumedLateFetchesCurrent(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	answered, kept := q.send("request answered meanwhile"), q.send("request still open")
	q.pass()
	first := wakeTokenOf(q.lastWake())
	if q.w.count() != 1 || !strings.Contains(q.lastWake(), "новые сообщения;") {
		t.Fatalf("first pointer: %v", q.w.calls)
	}
	// A person read and answered one of them in the app.
	if _, err := q.a.Ack("", AckRequest{IDs: []string{answered.ID}}); err != nil {
		t.Fatal(err)
	}
	newer := q.send("request newer than the pointer")
	q.age(queuedOwnerMax + time.Second) // a long time passes: the thread ran nothing
	if l, _ := q.a.leases.get(kept.ID); l.State != LeaseFailed || l.Reason != "queued_stale" {
		t.Fatalf("lease of the unconsumed pointer's message: %+v", l)
	}
	// Unproven for queuedOwnerMax, the first pointer keeps no other out.
	if l, _ := q.a.leases.get(newer.ID); q.w.count() != 2 || l.State != LeaseLeased || l.Token == first || l.Token != wakeTokenOf(q.lastWake()) {
		t.Fatalf("the newer message got no pointer of its own: %v %+v", q.w.calls, l)
	}

	q.consumed(path, first)
	page := q.fetch()
	if got := ids(page.Messages); !page.Delivered || !slices.Equal(got, []string{kept.ID, newer.ID}) {
		t.Fatalf("the late pointer's fetch: %v delivered %v, want %v", got, page.Delivered, []string{kept.ID, newer.ID})
	}
	for _, id := range []string{kept.ID, newer.ID} {
		if l, _ := q.a.leases.get(id); l.State != LeaseAcked {
			t.Fatalf("lease of %s after the fetch: %+v", id, l)
		}
	}
	if p, _ := q.a.Unread("", "", 100); p.Total != 0 {
		t.Fatalf("still unread after the fetch: %+v", p.Messages)
	}
	// A second call of that turn: nothing, and nothing changes.
	leases, sess, unread := q.state()
	if late := q.fetch(); late.Delivered || len(late.Messages) != 0 {
		t.Fatalf("the second call presented something again: %+v", late)
	}
	l2, s2, u2 := q.state()
	if !reflect.DeepEqual(leases, l2) || !reflect.DeepEqual(sess, s2) || !slices.Equal(unread, u2) {
		t.Fatalf("a fetch of nothing changed state: %+v -> %+v, %+v -> %+v", leases, l2, sess, s2)
	}
	q.pass()
	if q.w.count() != 2 {
		t.Fatalf("queued again with nothing unread: %v", q.w.calls)
	}
}

// The pointer's prompt in the rollout reads nothing. While the pointer is
// unconsumed, and for pointerFetchGrace after it ran, its message is the
// session's and gets no second prompt; no fetch within the grace releases it,
// visibly, and the next wake queues a new pointer for it.
func TestPointerUnfetchedReleases(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	m := q.send("request the turn never fetched")
	q.pass()
	token := wakeTokenOf(q.lastWake())
	q.age(inboxWakeGrace + time.Second)
	q.age(staleWakeWait(maxIdleWakes) + time.Second)
	if l, _ := q.a.leases.get(m.ID); q.w.count() != 1 || l.Attempts != 1 || l.State != LeaseLeased {
		t.Fatalf("a second prompt beside the unconsumed pointer: %v %+v", q.w.calls, l)
	}

	q.consumed(path, token, m.ID)
	l, _ := q.a.leases.get(m.ID)
	if s := q.session(); l.State != LeaseLeased || l.Owner != "s-q" || l.Shown.IsZero() || s.Woken || s.Wakes != 0 {
		t.Fatalf("after the pointer's prompt: %+v %+v", l, s)
	}
	if p, _ := q.a.Unread("", "", 100); p.Total != 1 {
		t.Fatalf("the marker read the message: %+v", p)
	}
	// Inside the grace: the session's, and no second prompt.
	q.pass()
	if got, _ := q.a.leases.get(m.ID); q.w.count() != 1 || got.State != LeaseLeased {
		t.Fatalf("inside the fetch grace: %v %+v", q.w.calls, got)
	}
	if _, err := q.a.RegisterSession(SessionRequest{SessionID: "s-other", Provider: ProviderCodex, Folder: q.dir, Wake: WakeNextEvent, Idle: true}); err != nil {
		t.Fatal(err)
	}
	if granted, _ := q.a.Claim(ClaimRequest{SessionID: "s-other", IDs: []string{m.ID}}); len(granted) != 0 {
		t.Fatalf("another session took it inside the grace: %v", granted)
	}
	if err := q.a.EndSession("s-other"); err != nil {
		t.Fatal(err)
	}

	// The grace passed with no fetch.
	q.a.leases.mu.Lock()
	q.a.leases.m[m.ID].Shown = time.Now().Add(-pointerFetchGrace - time.Second)
	q.a.leases.mu.Unlock()
	q.a.leaseSweep(time.Now())
	l, _ = q.a.leases.get(m.ID)
	if l.State != LeaseRetry || l.Reason != reasonUnfetched || l.Fails["s-q"] != 1 || !l.Shown.IsZero() || l.NextAt.IsZero() {
		t.Fatalf("lease after the grace: %+v", l)
	}
	waitAttempts(t, q.b, m, AttemptWakeRequested, AttemptHeld+":"+HoldUnfetched)
	// Never lost: unread, and its hooks' at their next event.
	if page, _ := q.a.UnreadFor(q.dir, "s-q", "", 10); page.Total != 1 || len(page.Woken) != 0 {
		t.Fatalf("an unfetched message is lost: %+v", page)
	}
	// Not before its backoff; then a new pointer, a new attempt.
	q.pass()
	if q.w.count() != 1 {
		t.Fatalf("queued again inside the backoff: %v", q.w.calls)
	}
	backoffOver(q.a, m.ID)
	q.pass()
	next := wakeTokenOf(q.lastWake())
	l, _ = q.a.leases.get(m.ID)
	if q.w.count() != 2 || next == token || !pointerToken(next) || l.State != LeaseLeased || l.Token != next || l.Attempts != 2 || !l.Shown.IsZero() {
		t.Fatalf("no new pointer after the release: %v %+v", q.w.calls, l)
	}
	// That one is fetched: delivered once.
	q.consumed(path, next, m.ID)
	if page := q.fetch(); !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
		t.Fatalf("the second pointer's fetch: %+v", page)
	}
	q.a.leaseSweep(time.Now().Add(pointerFetchGrace + time.Minute))
	if l, _ := q.a.leases.get(m.ID); l.State != LeaseAcked {
		t.Fatalf("a fetched message was released: %+v", l)
	}
}

// A pointer whose turn never fetches is bounded like every automatic lease:
// maxLeaseAttempts, then needs_human; never queued twice at once.
func TestPointerUnfetchedHandedAndBounded(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	take := func(token string) {
		t.Helper()
		if ok, err := b.take("m", "", "s-q", ViaQueue, token, now.Add(time.Minute), now); err != nil || !ok {
			t.Fatalf("take: %v %v", ok, err)
		}
	}
	take(pointerTokenPrefix + "0000000000000001")
	if !b.handedTo("m", "s-q", true) {
		t.Fatal("a queued pointer's message is not handed")
	}
	if ch, _ := b.shown("s-q", pointerTokenPrefix+"0000000000000001", now); !ch {
		t.Fatal("the pointer's prompt did not start the grace")
	}
	if ch, _ := b.shown("s-q", pointerTokenPrefix+"0000000000000001", now.Add(time.Minute)); ch {
		t.Fatal("the grace started twice")
	}
	none := func(string) bool { return false }
	idle := func(string) time.Time { return time.Time{} }
	if ends := b.due(now.Add(pointerFetchGrace-time.Second), none, idle); len(ends) != 0 || !b.handedTo("m", "s-q", true) {
		t.Fatalf("ended inside the grace: %+v", ends)
	}
	// Shown counts, not the queue's silence.
	ends := b.due(now.Add(queuedOwnerMax+time.Hour), none, idle)
	if len(ends) != 1 || ends[0].reason != reasonUnfetched {
		t.Fatalf("after the grace: %+v", ends)
	}
	for attempt := 1; ; attempt++ {
		failed, err := b.revoke("s-q", []string{"m"}, reasonUnfetched, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(failed) == 1 {
			if attempt != maxLeaseAttempts {
				t.Fatalf("failed after %d attempts, want %d", attempt, maxLeaseAttempts)
			}
			break
		}
		if attempt >= maxLeaseAttempts || b.handedTo("m", "s-q", true) {
			t.Fatalf("attempt %d: an unfetched pointer's message is still handed, or never fails", attempt)
		}
		take(pointerTokenPrefix + "000000000000001" + string(rune('1'+attempt)))
	}
	if l, _ := b.get("m"); l.State != LeaseFailed || !l.Failed || !b.blocked("m", "s-q", ViaQueue, now.Add(time.Hour)) {
		t.Fatalf("after every attempt: %+v", l)
	}
}

// A prompt a node before pointers queued carried its messages: after the
// upgrade its marker still starts and acknowledges them, and the session's
// hooks still find them woken, not listed.
func TestOldQueuedPromptAckedByMarker(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	m := q.send("request an older node queued with its text")
	// leases.json and sessions.json as v0.6.75 left them.
	var l Lease
	rec := `{"id":"` + m.ID + `","state":"leased","owner":"s-q","via":"queue","token":"9c503c66a1b2c3d4","attempts":1,"at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if err := json.Unmarshal([]byte(rec), &l); err != nil || !l.Shown.IsZero() || pointerToken(l.Token) {
		t.Fatalf("an old lease record: %+v %v", l, err)
	}
	q.a.leases.mu.Lock()
	q.a.leases.m[m.ID] = &l
	q.a.leases.mu.Unlock()
	q.a.sess.mu.Lock()
	s := q.a.sess.sessions["s-q"]
	s.Woken, s.Wakes, s.WokeAt = true, 1, time.Now()
	q.a.sess.mu.Unlock()
	q.a.restoreLeases(time.Now())
	if page, _ := q.a.UnreadFor(q.dir, "s-q", "", 10); page.Total != 0 || len(page.Woken) != 1 || page.Woken[0].WakeToken != l.Token {
		t.Fatalf("an old prompt's message is not woken with for its hooks: %+v", page)
	}
	q.pass()
	if q.w.count() != 0 {
		t.Fatalf("a pointer beside the old prompt: %v", q.w.calls)
	}
	q.a.resumeQueueObservers(t.Context())
	q.wrote(path, WakeMarker(l.Token)+"\n")
	eventually(t, "the old prompt's marker acknowledges", func() bool {
		got, _ := q.a.leases.get(m.ID)
		p, _ := q.a.Unread("", "", 100)
		s := q.session()
		return got.State == LeaseAcked && p.Total == 0 && !s.Woken
	})
}

// The fetch is the delivery only for a session the node wakes by pointers: any
// other session's listing, and a listing with no session, read nothing.
func TestFetchOnlyForQueueSessions(t *testing.T) {
	a, b := wakePair(t, &fakeWaker{ready: true})
	dir := t.TempDir()
	for _, r := range []SessionRequest{
		{SessionID: "s-q", Provider: ProviderCodex, Folder: dir, Wake: WakeQueue, Idle: true},
		{SessionID: "s-c", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake},
	} {
		if _, err := a.RegisterSession(r); err != nil {
			t.Fatal(err)
		}
	}
	m := ask(t, a, b, "request for whoever reads")
	for _, query := range []url.Values{
		{"agent": {"1"}, "session": {"s-c"}},  // a Claude session's MCP tool
		{"agent": {"1"}},                      // a tool outside a session
		{"session": {"s-q"}, "folder": {dir}}, // its hooks' listing
	} {
		page, err := a.UnreadQuery(query)
		if err != nil || page.Delivered || !slices.ContainsFunc(page.Messages, func(u UnreadMessage) bool { return u.ID == m.ID }) {
			t.Fatalf("listing %v: %+v %v", query, page, err)
		}
		if p, _ := a.Unread("", "", 100); p.Total != 1 {
			t.Fatalf("listing %v read the message", query)
		}
	}
	if _, ok := a.leases.get(m.ID); ok {
		t.Fatal("a listing took a lease")
	}
	// A message another live session's hook holds is not the fetching one's.
	if granted, _ := a.Claim(ClaimRequest{SessionID: "s-c", IDs: []string{m.ID}}); len(granted) != 1 {
		t.Fatalf("hook claim of the other session: %v", granted)
	}
	fetch := url.Values{"agent": {"1"}, "session": {"s-q"}}
	if page, err := a.UnreadQuery(fetch); err != nil || page.Delivered || len(page.Messages) != 0 {
		t.Fatalf("the fetch took another session's message: %+v %v", page, err)
	}
	// What it may take: delivered, read, the session's to answer.
	own := ask(t, a, b, "request for the queue session")
	page, err := a.UnreadQuery(fetch)
	if err != nil || !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != own.ID || page.Total != 1 {
		t.Fatalf("the queue session's fetch: %+v %v", page, err)
	}
	if rec, _ := a.chats.message(own.ID); rec.ReadAt.IsZero() || rec.Assigned != "session:s-q" {
		t.Fatalf("the fetched request: read at %v, assigned %q", rec.ReadAt, rec.Assigned)
	}
	if p, _ := a.Unread("", "", 100); p.Total != 1 || p.Messages[0].ID != m.ID {
		t.Fatalf("unread after the fetch: %+v", p.Messages)
	}
	if l, _ := a.leases.get(own.ID); l.State != LeaseAcked || l.Via != ViaHook || l.Attempts != 0 {
		t.Fatalf("lease of a fetched message: %+v", l)
	}
}

// The pointer names the MCP tool with the session's id as its argument (a
// client's call may name no thread) and, for a thread without the tool, the
// exact CLI command with the node's own executable and that id. No folder: a
// path would be text the agent's shell expands.
func TestPointerPromptText(t *testing.T) {
	got := PointerPrompt("019f-thread", "p-0123456789abcdef", `C:\Program Files\agentlink\agentlink.exe`)
	want := "agent-link: для этой сессии есть новые сообщения; их текста здесь нет. [agent-link wake p-0123456789abcdef]\n" +
		"Получите их сейчас: вызовите инструмент unread MCP-сервера agentlink с одним аргументом session: \"019f-thread\". " +
		"Если вызов отклонён из-за этого аргумента (инструмент его не принимает), вызовите unread ещё раз без аргументов. " +
		"Если такого инструмента нет или в его ответе есть сообщения, но нет delivered: true, выполните команду (в PowerShell начните её с &): " +
		`"C:\Program Files\agentlink\agentlink.exe" chat unread --session 019f-thread` + "\n" +
		"Вызов выдаёт сообщения, которые ждут эту сессию сейчас, и сразу отмечает их прочитанными: прочитайте их и, где просят ответа, ответьте. " +
		"Если в ответе есть note, выдана только часть: повторяйте тот же вызов с теми же аргументами (без after), пока note не исчезнет. " +
		"Если он ничего не вернул и сообщений agent-link нет в контексте этого хода, делать нечего: ответьте одной короткой строкой и завершите ход."
	if got != want {
		t.Fatalf("pointer prompt:\n%s\nwant:\n%s", got, want)
	}
	if !validWakeToken("p-0123456789abcdef") || pointerToken(randomHex(8)) || !pointerToken(pointerTokenPrefix+randomHex(8)) {
		t.Fatal("pointer tokens are not told from the others")
	}
	if got := PointerPrompt("s", "p-0123456789abcdef", ""); !strings.Contains(got, "&): agentlink chat unread --session s\n") {
		t.Fatalf("without an executable: %s", got)
	}
	// The node queues it with its own executable (cliExecutable, as Render's CLI line).
	old := cliExecutable
	cliExecutable = func() string { return `C:\apps\agentlink.exe` }
	t.Cleanup(func() { cliExecutable = old })
	q := newStaleQueue(t)
	q.send("request")
	q.pass()
	if text := q.w.texts[0]; !strings.Contains(text, `&): "C:\apps\agentlink.exe" chat unread --session s-q`+"\n") || !strings.Contains(text, `session: "s-q". `) {
		t.Fatalf("the queued pointer's CLI line: %s", text)
	}
}

// A folder's name is no part of the prompt: one a shell would expand
// (PowerShell's $(...) and backtick, quotes, spaces) reaches no agent's shell
// through it, and the call that names no folder still takes the session's
// messages (the node knows its folder).
func TestPointerPromptHasNoFolder(t *testing.T) {
	name := "it's $(Start-Process calc) `whoami` & a b"
	if runtime.GOOS != "windows" {
		name += ` "q" $(touch pwned)`
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	q := newStaleQueueIn(t, dir)
	m := q.send("request for a session in an odd folder")
	q.pass()
	if q.w.count() != 1 {
		t.Fatalf("no pointer: %v", q.w.calls)
	}
	text := q.w.texts[0]
	for _, bad := range []string{name, dir, "$(", "`", "'", "--folder", "Start-Process"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the pointer's prompt carries %q of its folder: %s", bad, text)
		}
	}
	if !strings.Contains(text, " chat unread --session s-q\n") {
		t.Fatalf("the pointer's command: %s", text)
	}
	// The command as the prompt gives it: a session, no folder.
	if page := q.fetch(); !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
		t.Fatalf("the fetch without a folder: %+v", page)
	}
}

// One answer of a fetch carries what a hook's batch does (FitUnread), and only
// that is acknowledged: the rest stays unread, the answer says so in plain
// words (and names no cursor), and the same call made again takes it, each
// message once; a cursor the caller sends all the same is ignored.
func TestFetchFitsBudget(t *testing.T) {
	q := newStaleQueue(t)
	q.rollout("")
	const total = 12
	var sent []string
	for i := range total {
		sent = append(sent, q.send(strings.Repeat("я", 2400)+" "+string(rune('a'+i))).ID)
	}
	var got []string
	for call := 1; len(got) < total; call++ {
		page := q.fetch()
		left, _ := q.a.Unread("", "", 100)
		runes := 0
		for _, m := range page.Messages {
			runes += utf8.RuneCountInString(FormatUnread(m))
		}
		if !page.Delivered || len(page.Messages) == 0 || len(page.Messages) != FitUnread(page.Messages) || runes > FormatBudget {
			t.Fatalf("call %d: %d messages, %d runes (budget %d), delivered %v", call, len(page.Messages), runes, FormatBudget, page.Delivered)
		}
		got = append(got, ids(page.Messages)...)
		if left.Total != total-len(got) {
			t.Fatalf("call %d: %d unread after %d delivered of %d", call, left.Total, len(got), total)
		}
		for _, m := range left.Messages {
			if slices.Contains(got, m.ID) {
				t.Fatalf("call %d: %s is in an answer and still unread", call, m.ID)
			}
		}
		more := len(got) < total
		if page.Next != "" || (page.Note != "") != more {
			t.Fatalf("call %d: next %q note %q with %d left", call, page.Next, page.Note, total-len(got))
		}
		if more && !strings.Contains(page.Note, fmt.Sprintf("ещё %d ждут", total-len(got))) {
			t.Fatalf("call %d: the note does not say what is left: %q", call, page.Note)
		}
		if call > total {
			t.Fatal("the fetch never ends")
		}
	}
	if !slices.Equal(got, sent) {
		t.Fatalf("delivered %v, want each of %v once, in order", got, sent)
	}
	if page := q.fetch(); page.Delivered || len(page.Messages) != 0 || page.Note != "" || page.Next != "" {
		t.Fatalf("a fetch with nothing left: %+v", page)
	}
	// A cursor past everything hides nothing from a fetch (issue #41): a plain
	// listing with it is empty, the session's own call delivers.
	late := q.send("after a stale cursor")
	listed, err := q.a.Unread("", "", 100)
	if err != nil || len(listed.Messages) != 1 {
		t.Fatalf("the listing: %+v %v", listed, err)
	}
	past := listed.Messages[0].Cursor
	if plain, err := q.a.UnreadQuery(url.Values{"after": {past}}); err != nil || len(plain.Messages) != 0 || plain.Delivered {
		t.Fatalf("a plain listing after the last cursor: %+v %v", plain, err)
	}
	page, err := q.a.UnreadQuery(url.Values{"agent": {"1"}, "session": {"s-q"}, "after": {past}})
	if err != nil || !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != late.ID || page.Next != "" {
		t.Fatalf("a fetch with a stale cursor: %+v %v", page, err)
	}
}

// One pointer at a time. While a pointer is in the thread's queue and its
// rollout has not shown it, nothing more is queued for that thread: the
// messages that come meanwhile are leased to it under that pointer (their
// authors hear of the wake, on the schedule of the idle period's wakes), and
// the pointer's turn fetches them all.
func TestOnePointerAtATime(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	first := q.send("first request")
	q.pass()
	token := wakeTokenOf(q.lastWake())
	if q.w.count() != 1 || !pointerToken(token) || q.pending() != token {
		t.Fatalf("first pointer: %v", q.w.calls)
	}
	second := q.send("second request")
	q.pass() // inside the wait of the idle period's next wake: not leased yet
	if _, ok := q.a.leases.get(second.ID); ok || q.w.count() != 1 {
		t.Fatalf("leased before a wake was due: %v", q.w.calls)
	}
	q.age(inboxWakeGrace + time.Second)
	third := q.send("third request")
	q.age(staleWakeWait(maxIdleWakes) + time.Second)
	if q.w.count() != 1 {
		t.Fatalf("a second prompt beside the unconsumed pointer: %v", q.w.calls)
	}
	if s := q.session(); !s.Woken || s.Wakes != 3 {
		t.Fatalf("the wakes of the idle period are not counted: %+v", s)
	}
	for _, m := range []Message{first, second, third} {
		if l, _ := q.a.leases.get(m.ID); l.State != LeaseLeased || l.Owner != "s-q" || l.Via != ViaQueue || l.Token != token || l.Attempts != 1 {
			t.Fatalf("lease of %q: %+v, want the first pointer's %s", m.Body, l, token)
		}
		waitAttempts(t, q.b, m, AttemptWakeRequested)
	}

	// The pointer runs: its marker covers all three, its fetch delivers them.
	q.consumed(path, token)
	for _, m := range []Message{first, second, third} {
		if l, _ := q.a.leases.get(m.ID); l.State != LeaseLeased || l.Shown.IsZero() {
			t.Fatalf("lease of %q after the pointer's prompt: %+v", m.Body, l)
		}
	}
	if q.pending() != "" {
		t.Fatal("a pointer the rollout shows still keeps the queue closed")
	}
	page := q.fetch()
	if got := ids(page.Messages); !page.Delivered || !slices.Equal(got, []string{first.ID, second.ID, third.ID}) || page.Note != "" {
		t.Fatalf("the pointer's fetch: %v delivered %v note %q", got, page.Delivered, page.Note)
	}
	for _, m := range []Message{first, second, third} {
		if l, _ := q.a.leases.get(m.ID); l.State != LeaseAcked {
			t.Fatalf("lease of %q after the fetch: %+v", m.Body, l)
		}
		waitAttempts(t, q.b, m, AttemptWakeRequested, AttemptWokenConfirmed)
	}
	// Consumed: the next message gets a pointer of its own.
	q.send("fourth request")
	q.pass()
	if next := wakeTokenOf(q.lastWake()); q.w.count() != 2 || next == token || !strings.Contains(q.lastWake(), "[pointer for] fourth request") {
		t.Fatalf("no new pointer after the first was consumed: %v", q.w.calls)
	}
}

// A pointer that stays unconsumed keeps the bounds of every queue wake: what
// was leased to it fails unproven after queuedOwnerMax (needs_human), the
// wakes of the idle period are spent on the same schedule, and what waits
// past them is reported once. While it is inside queuedOwnerMax nothing more
// is queued; unproven past it, it is lost for the node, and the stale wake
// that is due then queues one new pointer. A pointer the rollout shows though
// no observer saw it opens the queue again too.
func TestUnconsumedPointerKeepsBounds(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	first := q.spend()
	token := wakeTokenOf(q.lastWake())
	limit := maxIdleWakes + maxStaleWakes
	var covered []Message
	for i := maxIdleWakes; i < limit; i++ {
		covered = append(covered, q.send(fmt.Sprintf("stale request %d", i)))
		q.age(staleWakeWait(i) + time.Second)
		if want := stalePrompts(i); q.w.count() != want {
			t.Fatalf("stale wake %d: %d prompts, want %d", i+1, q.w.count(), want)
		}
	}
	second := wakeTokenOf(q.lastWake())
	if second == token || !pointerToken(second) {
		t.Fatalf("no new pointer once the first was unproven for queuedOwnerMax: %v", q.w.calls)
	}
	waiting := q.send("request past the cap")
	q.age(queuedOwnerMax + time.Second)
	if s := q.session(); q.w.count() != 2 || s.Wakes != limit || !s.WakeCapped {
		t.Fatalf("past the cap: prompts %d %+v", q.w.count(), s)
	}
	for i, m := range append([]Message{first}, covered...) {
		want := token
		if i == len(covered) {
			want = second // the wake that found the first pointer lost
		}
		l, _ := q.a.leases.get(m.ID)
		if l.State != LeaseFailed || l.Reason != "queued_stale" || l.Token != want || l.Attempts != 1 {
			t.Fatalf("lease of %q: %+v, want token %s", m.Body, l, want)
		}
		waitAttempts(t, q.b, m, AttemptWakeRequested, AttemptNeedsHuman)
	}
	waitAttempts(t, q.b, waiting, AttemptNeedsHuman)
	if q.pending() != "" {
		t.Fatal("a pointer whose leases failed unproven still keeps the queue closed")
	}

	// The thread runs it at last, with no observer of the node watching: a
	// new idle period's message is the next pointer's.
	q.wrote(path, WakeMarker(token)+"\n")
	q.reg(false)
	q.reg(true)
	late := q.send("request after the pointer ran")
	q.pass()
	if next := wakeTokenOf(q.lastWake()); q.w.count() != 3 || next == token || next == second || !strings.Contains(q.lastWake(), "[pointer for] "+late.Body) {
		t.Fatalf("no new pointer after the old one ran: %v", q.w.calls)
	}
}

// stalePrompts is how many prompts a thread that proves nothing has in its
// queue after wake i+1 of its idle period, each wake aged by its own wait
// (staleQueue.age makes every open lease as old): the one pointer while that
// is inside queuedOwnerMax, a second one from the wake that finds it unproven
// past it (lost).
func stalePrompts(i int) int {
	if i >= maxIdleWakes && staleWakeWait(i) >= queuedOwnerMax {
		return 2
	}
	return 1
}

// idleFor lets d pass in which the thread did nothing (see hookSilence).
func (q *staleQueue) idleFor(d time.Duration) {
	q.t.Helper()
	hookSilence(q.a.Node, "s-q", d)
}

// hookSilence makes session's last hook events (the turn it took, its end)
// and its leases d older: d passed in which it reported and proved nothing.
func hookSilence(n *Node, session string, d time.Duration) {
	n.sess.mu.Lock()
	if s := n.sess.sessions[session]; s != nil {
		if !s.TurnAt.IsZero() {
			s.TurnAt = s.TurnAt.Add(-d)
		}
		s.IdleAt = s.IdleAt.Add(-d)
	}
	n.sess.mu.Unlock()
	n.leases.mu.Lock()
	for _, l := range n.leases.m {
		if l.Owner == session {
			l.At = l.At.Add(-d)
		}
	}
	n.leases.mu.Unlock()
}

// The field case of a pointer Codex lost (it is never in the rollout): its
// message fails unproven after queuedOwnerMax, and from then on it keeps no
// other pointer out. Every later idle period's request gets a pointer of its
// own, days on, whatever the book still holds of the lost ones.
func TestLostPointerDoesNotBlockLaterPointers(t *testing.T) {
	q := newStaleQueue(t)
	q.rollout("")
	first := q.send("first request")
	q.pass()
	lost := wakeTokenOf(q.lastWake())
	q.age(queuedOwnerMax + time.Second)
	if l, _ := q.a.leases.get(first.ID); l.State != LeaseFailed || l.Reason != "queued_stale" || q.pending() != "" {
		t.Fatalf("the lost pointer's message: %+v, pending %q", l, q.pending())
	}
	seen := map[string]bool{lost: true}
	const days = 10
	for day := 1; day <= days; day++ {
		// The thread took a turn, is idle again, and a new request comes.
		q.reg(false)
		q.reg(true)
		m := q.send(fmt.Sprintf("request of day %d", day))
		q.pass()
		token := wakeTokenOf(q.lastWake())
		if l, _ := q.a.leases.get(m.ID); q.w.count() != day+1 || seen[token] || l.State != LeaseLeased || l.Token != token || q.pending() != token {
			t.Fatalf("day %d: prompts %d, token %s (seen %v), lease %+v", day, q.w.count(), token, seen[token], l)
		}
		seen[token] = true
		// It is lost as well: failed unproven, reported, and a day passes, in
		// which the book is pruned.
		q.age(queuedOwnerMax + time.Second)
		waitAttempts(t, q.b, m, AttemptWakeRequested, AttemptNeedsHuman)
		q.a.deliv.mu.Lock()
		q.a.deliv.leasePruned = time.Time{}
		q.a.deliv.mu.Unlock()
		q.a.leases.mu.Lock()
		for _, l := range q.a.leases.m {
			l.At = l.At.Add(-24 * time.Hour)
		}
		q.a.leases.mu.Unlock()
		q.reg(true)
		q.a.leaseSweep(time.Now())
	}
	// The lost pointers' messages are no wake's any more, and still unread for
	// a person, a hook or a later pointer's fetch.
	if l, _ := q.a.leases.get(first.ID); l.State != LeaseFailed || l.Attempts != 1 {
		t.Fatalf("the first lost message: %+v", l)
	}
	if page := q.fetch(); !page.Delivered || len(page.Messages) != days+1 {
		t.Fatalf("a fetch after the lost pointers: %d messages, delivered %v", len(page.Messages), page.Delivered)
	}
}

// A pointer the thread did not run in a turn it took since: its hooks reported
// a turn after the pointer was queued and that turn's end, and the rollout has
// no marker of it. For pointerLostWait from that end nothing is queued or
// leased (the thread may run the pointer next); then the pointer is lost, its
// message goes back to retry (pointer_lost), and the next wake queues a new
// pointer, for the newer message at once and for the lost one's after its
// backoff, under that new pointer. One the thread did run in that time is
// shown, not lost.
func TestPointerNotRunByALaterTurnIsLost(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	old := q.send("request of the lost pointer")
	q.pass()
	lost := wakeTokenOf(q.lastWake())
	// An idle event with no turn before it (the app was opened again) loses nothing.
	q.reg(true)
	q.idleFor(pointerLostWait + time.Second)
	newer := q.send("request after the turn")
	q.age(inboxWakeGrace + time.Second)
	if q.w.count() != 1 || q.pending() != lost || !q.covers(lost, newer) {
		t.Fatalf("an idle event alone lost the pointer: %v pending %q", q.w.calls, q.pending())
	}
	// The thread takes a turn of its own and ends it.
	q.reg(false)
	q.reg(true)
	after := q.send("request after that turn")
	q.pass()
	if _, ok := q.a.leases.get(after.ID); ok || q.w.count() != 1 || q.pending() != lost {
		t.Fatalf("queued or leased while the pointer may still run: %v", q.w.calls)
	}
	q.idleFor(pointerLostWait + time.Second)
	q.pass()
	next := wakeTokenOf(q.lastWake())
	if q.w.count() != 2 || next == lost || !pointerToken(next) || q.pending() != next || !strings.Contains(q.lastWake(), "[pointer for] "+after.Body) {
		t.Fatalf("no new pointer after the lost one: %v", q.w.calls)
	}
	for _, m := range []Message{old, newer} {
		l, _ := q.a.leases.get(m.ID)
		if l.State != LeaseRetry || l.Reason != reasonLost || l.Attempts != 1 || l.Failed {
			t.Fatalf("lease of %q after its pointer was lost: %+v", m.Body, l)
		}
		if q.a.leases.handedTo(m.ID, "s-q", true) || strings.Contains(q.lastWake(), "[pointer for] "+m.Body) {
			t.Fatalf("%q is handed still, or named by two pointers at once: %v", m.Body, q.w.calls)
		}
		backoffOver(q.a, m.ID)
	}
	// Their next wake is a lease under the new pointer: no third prompt.
	q.age(inboxWakeGrace + time.Second)
	for _, m := range []Message{old, newer} {
		if l, _ := q.a.leases.get(m.ID); q.w.count() != 2 || l.State != LeaseLeased || l.Token != next || l.Attempts != 2 {
			t.Fatalf("lease of %q under the new pointer: %d prompts %+v", m.Body, q.w.count(), l)
		}
	}
	// The new pointer runs behind a further turn, inside the wait: shown, not
	// lost, and its fetch delivers all three.
	q.reg(false)
	q.reg(true)
	q.wrote(path, WakeMarker(next)+"\n")
	q.idleFor(pointerLostWait + time.Second)
	q.pass()
	if l, _ := q.a.leases.get(after.ID); l.State != LeaseLeased || l.Shown.IsZero() || q.pending() != "" || q.w.count() != 2 {
		t.Fatalf("a pointer the rollout shows was taken for lost: %+v %v", l, q.w.calls)
	}
	if page := q.fetch(); !page.Delivered || !slices.Equal(ids(page.Messages), []string{old.ID, newer.ID, after.ID}) {
		t.Fatalf("the new pointer's fetch: %v", ids(page.Messages))
	}
}

// The pointer ran and its turn fetched one page of several, then stopped: what
// it left unread is released after the fetch grace and gets a new pointer once
// its backoff is over.
func TestPointerStoppedAfterFirstPage(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	var sent []Message
	for i := range 6 {
		sent = append(sent, q.send(strings.Repeat("я", 2400)+" "+string(rune('a'+i))))
	}
	q.pass()
	token := wakeTokenOf(q.lastWake())
	q.consumed(path, token)
	page := q.fetch()
	if n := len(page.Messages); n == 0 || n == len(sent) || page.Note == "" {
		t.Fatalf("the first page: %d of %d messages, note %q", n, len(sent), page.Note)
	}
	rest := sent[len(page.Messages):]
	q.pass()
	if l, _ := q.a.leases.get(rest[0].ID); q.w.count() != 1 || l.State != LeaseLeased || l.Shown.IsZero() {
		t.Fatalf("inside the fetch grace: %d prompts %+v", q.w.count(), l)
	}
	q.a.leases.mu.Lock()
	for _, l := range q.a.leases.m {
		if !l.Shown.IsZero() {
			l.Shown = l.Shown.Add(-pointerFetchGrace - time.Second)
		}
	}
	q.a.leases.mu.Unlock()
	q.pass()
	if l, _ := q.a.leases.get(rest[0].ID); q.w.count() != 1 || l.State != LeaseRetry || l.Reason != reasonUnfetched {
		t.Fatalf("after the fetch grace: %d prompts %+v", q.w.count(), l)
	}
	for _, m := range rest {
		backoffOver(q.a, m.ID)
		waitAttempts(t, q.b, m, AttemptWakeRequested, AttemptHeld+":"+HoldUnfetched)
	}
	q.pass()
	if l, _ := q.a.leases.get(rest[0].ID); q.w.count() != 2 || l.State != LeaseLeased || l.Token == token || l.Attempts != 2 {
		t.Fatalf("no new pointer for the remainder: %d prompts %+v", q.w.count(), l)
	}
}

// A seat's message a pointer was queued for is delivered by the seat
// session's fetch, once: listed (a pointer's prompt has no message), claimed
// through its wake mark, read by the seat; no second pointer follows.
func TestPointerFetchDeliversSeatMessage(t *testing.T) {
	l := &seatLauncher{}
	dir, home := t.TempDir(), t.TempDir()
	w := &fakeWaker{ready: true}
	a := newProjectNodeAt(t, "morgott", newTestProject(t), t.TempDir(), listen(t), nil)
	a.SetFolders(dir, nil)
	a.SetLauncher(l, ProviderClaude)
	a.SetSessionWaker(w)
	a.wakeEvery = time.Hour
	a.seatBusy = l.busy
	a.start(t)
	_, codex := addSeats(t, a)
	if _, err := a.RegisterSession(SessionRequest{SessionID: codex.SessionID, Provider: codex.Provider, Folder: dir, Wake: WakeQueue,
		Idle: true, CodexHome: home, TTLSec: staleQueueTTL}); err != nil {
		t.Fatal(err)
	}
	a.syncQueueWake()
	chat, _ := a.NewProjectChat(nil)
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "review this seat", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}})
	if err != nil {
		t.Fatal(err)
	}
	a.wakeIdle(t.Context())
	token := wakeTokenOf(w.texts[0])
	key := leaseKey(codex.ID, m.ID)
	if lv, _ := a.leases.get(key); w.count() != 1 || !pointerToken(token) || strings.Contains(w.texts[0], m.Body) ||
		lv.State != LeaseLeased || lv.Owner != codex.SessionID || lv.Via != ViaQueue || lv.Token != token {
		t.Fatalf("the seat's pointer: %v %+v", w.calls, lv)
	}
	fetch := url.Values{"agent": {"1"}, "session": {codex.SessionID}}
	page, err := a.UnreadQuery(fetch)
	if err != nil || !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != m.ID || page.Messages[0].ForSeat != codex.ID || len(page.Woken) != 0 {
		t.Fatalf("the seat session's fetch: %+v %v", page, err)
	}
	if lv, _ := a.leases.get(key); lv.State != LeaseAcked {
		t.Fatalf("the seat's lease after the fetch: %+v", lv)
	}
	if again, err := a.UnreadQuery(fetch); err != nil || again.Delivered || len(again.Messages) != 0 || len(again.Woken) != 0 {
		t.Fatalf("the seat's message was delivered twice: %+v %v", again, err)
	}
	if left, _ := a.UnreadFor(dir, codex.SessionID, "", 10); len(left.Messages) != 0 || len(left.Woken) != 0 {
		t.Fatalf("still the seat session's after its fetch: %+v", left)
	}
	// Nothing waits for the seat: no second pointer, whatever lapses.
	past := time.Now().Add(-staleWakeWait(maxIdleWakes) - time.Second)
	a.seats.mu.Lock()
	for k, h := range a.seats.marks {
		h.at = past
		a.seats.marks[k] = h
	}
	a.seats.mu.Unlock()
	a.sess.mu.Lock()
	if s := a.sess.sessions[codex.SessionID]; s.Woken {
		s.WokeAt = past
	}
	a.sess.mu.Unlock()
	a.leaseSweep(time.Now())
	a.wakeIdle(t.Context())
	if w.count() != 1 {
		t.Fatalf("a second pointer for a delivered seat message: %v", w.calls)
	}
}

// What a Claude session's inbox wake, a seat's turn and the hooks render is
// what v0.6.75 rendered, to the byte: pointers changed the queue prompt only.
func TestRenderUnchangedBesidePointers(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	a := UnreadMessage{AsksYou: true, Cursor: "0001"}
	a.ID, a.ChatID, a.From, a.AuthorKind, a.Body, a.Participants, a.CreatedAt = "id1", "c1", "KPECTIK", "agent", "hello", []string{"KPECTIK", "me"}, at
	b := UnreadMessage{Cursor: "0002"}
	b.ID, b.From, b.AuthorKind, b.Body, b.CreatedAt = "id2", "peer", "human", "fyi", at
	msgs := []UnreadMessage{a, b}
	const (
		head1 = "От KPECTIK (агент) в чате c1 (участники: KPECTIK, me), id id1, {AT}:\nhello\n"
		ask1  = "Просит ответа от вас. Ответить: agentlink send --chat c1 --reply-to id1 --body-file <файл>\n\n"
		msg2  = "От peer (человек), id id2, {AT}:\nfyi\nК сведению, ответ не обязателен. Ответить: agentlink send --to peer --reply-to id2 --body-file <файл>"
		wake  = "agent-link: новые сообщения (2). Вся переписка остаётся в истории чата. [agent-link wake tok12345]\n\n"
		rest  = "\n\nЕщё 3 непрочитанных придут со следующим событием. Прочитать сейчас: agentlink chat unread --folder \"C:\\\\work\" --after 0002 ; прочитанные подтвердить: agentlink chat ack --ids <id,...> --session s"
	)
	golden := map[string]string{
		"inbox": wake + head1 + ask1 + msg2 + "\n\nЕщё 2 непрочитанных: agentlink chat unread --folder \"C:\\\\work\"",
		"seat": wake + head1 + "Просит ответа от вас. Ответ — ваше итоговое сообщение в этом ходе: agent-link сам отправит его в чат ответом на это сообщение. " +
			"Для ответа agentlink send не запускайте (иначе ответ уйдёт дважды); send — только чтобы спросить другого агента.\n\n" + msg2,
		"render:": "agent-link: непрочитанные сообщения для этой папки (5). Вся переписка остаётся в истории чата.\n\n" + head1 + ask1 + msg2 + rest,
		"render:stop": "Пока вы работали, пришли сообщения agent-link. Прочитайте их и, где просят ответа, ответьте; затем завершайте.\n\n" +
			head1 + ask1 + msg2 + rest,
		"render:wake": wake + head1 + ask1 + msg2 + "\n\nЕщё 3 непрочитанных: agentlink chat unread --folder \"C:\\\\work\"",
		"render:nudge": "agent-link: выше в этом разговоре уже есть новые сообщения agent-link (5): они пришли под конец прошлого хода, их ещё никто не читал. " +
			"Прочитайте их и, где просят ответа, ответьте. [agent-link wake tok12345]",
	}
	got := map[string]string{
		"inbox": WakePrompt(msgs, 2, `C:\work`, "tok12345"),
		"seat":  wakePrompt(msgs, 0, `C:\work`, "tok12345", true),
	}
	for _, mode := range []string{RenderBatch, RenderStop, RenderWake, RenderNudge} {
		r, err := Render(RenderRequest{Messages: msgs, Total: 5, Mode: mode, Token: "tok12345", Folder: `C:\work`, SessionID: "s", Kept: true})
		if err != nil {
			t.Fatal(err)
		}
		got["render:"+mode] = r.Text
		if want := "agent-link: 2 сообщения от KPECTIK, peer — беру в работу"; mode != RenderNudge && r.Notice != want {
			t.Errorf("notice of mode %q: %q", mode, r.Notice)
		}
	}
	for name, want := range golden {
		if want = strings.ReplaceAll(want, "{AT}", humantime.Format(at)); got[name] != want {
			t.Errorf("%s changed:\n%q\nwant\n%q", name, got[name], want)
		}
	}
}
