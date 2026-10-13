package node

import (
	"encoding/json"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	q.a.observeQueueStart(q.t.Context(), q.session(), token, ids) // unless the wake's own observer still runs
	eventually(q.t, "the pointer's observer ended", func() bool { _, on := q.a.observed.Load("s-q\x00" + token); return !on })
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
// lease failed unproven long before.
func TestPointerConsumedLateFetchesCurrent(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	answered, kept := q.send("request answered meanwhile"), q.send("request still open")
	q.pass()
	first := wakeTokenOf(q.lastWake())
	if q.w.count() != 1 || !strings.Contains(q.lastWake(), "новые сообщения (2)") {
		t.Fatalf("first pointer: %v", q.w.calls)
	}
	// A person read and answered one of them in the app.
	if _, err := q.a.Ack("", AckRequest{IDs: []string{answered.ID}}); err != nil {
		t.Fatal(err)
	}
	newer := q.send("request newer than the pointer")
	q.age(queuedOwnerMax + time.Second) // hours pass: the thread ran nothing
	if l, _ := q.a.leases.get(kept.ID); l.State != LeaseFailed || l.Reason != "queued_stale" {
		t.Fatalf("lease of the unconsumed pointer's message: %+v", l)
	}
	second := wakeTokenOf(q.lastWake())
	if q.w.count() != 2 || second == first || !strings.Contains(q.lastWake(), "[pointer for] "+newer.Body) || strings.Contains(q.lastWake(), kept.Body) {
		t.Fatalf("the newer message's own pointer: %v", q.w.calls)
	}

	q.consumed(path, first, answered.ID, kept.ID)
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
	// The second pointer runs after it: one trivial turn.
	leases, sess, unread := q.state()
	q.consumed(path, second, newer.ID)
	if late := q.fetch(); late.Delivered || len(late.Messages) != 0 {
		t.Fatalf("the second pointer presented something again: %+v", late)
	}
	l2, s2, u2 := q.state()
	if !reflect.DeepEqual(leases, l2) || !reflect.DeepEqual(sess, s2) || !slices.Equal(unread, u2) {
		t.Fatalf("the second pointer changed state: %+v -> %+v, %+v -> %+v", leases, l2, sess, s2)
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
	if ch, _ := b.shown("s-q", pointerTokenPrefix+"0000000000000001", []string{"m"}, now); !ch {
		t.Fatal("the pointer's prompt did not start the grace")
	}
	if ch, _ := b.shown("s-q", pointerTokenPrefix+"0000000000000001", []string{"m"}, now.Add(time.Minute)); ch {
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

// The pointer names the MCP tool and, for a thread without it, the exact CLI
// command with the node's own executable, the session and its folder.
func TestPointerPromptText(t *testing.T) {
	got := PointerPrompt(3, `C:\work dir`, "019f-thread", "p-0123456789abcdef", `C:\Program Files\agentlink\agentlink.exe`)
	want := "agent-link: для этой сессии есть новые сообщения (3); их текста здесь нет. [agent-link wake p-0123456789abcdef]\n" +
		"Получите их сейчас: вызовите инструмент unread MCP-сервера agentlink (без аргументов). " +
		"Если такого инструмента нет, выполните команду (в PowerShell начните её с &): " +
		`"C:\Program Files\agentlink\agentlink.exe" chat unread --session 019f-thread --folder "C:\work dir"` + "\n" +
		"Вызов выдаёт сообщения, которые ждут эту сессию сейчас, и сразу отмечает их прочитанными: прочитайте их и, где просят ответа, ответьте. " +
		"Если он ничего не вернул и сообщений agent-link нет в контексте этого хода, делать нечего: ответьте одной короткой строкой и завершите ход."
	if got != want {
		t.Fatalf("pointer prompt:\n%s\nwant:\n%s", got, want)
	}
	if !validWakeToken("p-0123456789abcdef") || pointerToken(randomHex(8)) || !pointerToken(pointerTokenPrefix+randomHex(8)) {
		t.Fatal("pointer tokens are not told from the others")
	}
	if got := PointerPrompt(1, "", "s", "p-0123456789abcdef", ""); !strings.Contains(got, "&): agentlink chat unread --session s\n") {
		t.Fatalf("without an executable or folder: %s", got)
	}
	// The node queues it with its own executable (cliExecutable, as Render's CLI line).
	old := cliExecutable
	cliExecutable = func() string { return `C:\apps\agentlink.exe` }
	t.Cleanup(func() { cliExecutable = old })
	q := newStaleQueue(t)
	q.send("request")
	q.pass()
	if text := q.w.texts[0]; !strings.Contains(text, `&): "C:\apps\agentlink.exe" chat unread --session s-q --folder "`+q.session().Folder+`"`+"\n") {
		t.Fatalf("the queued pointer's CLI line: %s", text)
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
