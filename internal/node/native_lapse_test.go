package node

import (
	"context"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// moduleSilent makes session s-n's module unheard for d: no registration or
// heartbeat of it came, whatever its shell hooks did meanwhile.
func moduleSilent(a *testNode, d time.Duration) {
	a.sess.mu.Lock()
	a.sess.sessions["s-n"].NativeSeen = time.Now().Add(-d)
	a.sess.mu.Unlock()
}

// A module that stops (refused at a reload, disabled, crashed) while the
// session's shell hooks keep firing does not leave the session deaf: only the
// module's own registrations keep it native, and after three of its heartbeats
// without one the shell hooks deliver again. When the module is back they
// stand down again, and nothing reaches the session twice.
func TestNativeLapsesWhenTheModuleIsSilent(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	const ttl = 900
	native := SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, Native: true, TTLSec: ttl}
	classic := SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, TTLSec: ttl}
	if _, err := a.RegisterSession(native); err != nil {
		t.Fatal(err)
	}
	m := ask(t, a, b, "the module claims it and dies")
	// The module took it for a turn and never acknowledged it.
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true}); len(got) != 1 {
		t.Fatalf("the module's claim: %v", got)
	}
	// Shell hook events alone, for less than three heartbeats: still the module's.
	moduleSilent(a, nativeSeenTTL-10*time.Second)
	if s, err := a.RegisterSession(classic); err != nil || !s.Native {
		t.Fatalf("a hook's registration within the module's time: %+v %v", s, err)
	}
	if page, _ := a.UnreadQuery(nativeQuery(dir)); page.Total != 0 || !a.sessionNative("s-n") {
		t.Fatalf("the shell hooks got %+v while the module is heard", page)
	}
	// Three heartbeats of the module missed; the shell hooks kept the session
	// live all along (each of their events registers).
	for range 3 {
		a.sess.mu.Lock()
		a.sess.sessions["s-n"].LastSeen = time.Now().Add(-(ttl - 1) * time.Second)
		a.sess.mu.Unlock()
		if _, err := a.RegisterSession(classic); err != nil {
			t.Fatal(err)
		}
	}
	moduleSilent(a, nativeSeenTTL+time.Second)
	page, err := a.UnreadQuery(nativeQuery(dir))
	if err != nil || page.Total != 1 || page.Messages[0].ID != m.ID || a.sessionNative("s-n") {
		t.Fatalf("module silent, shell hooks alive: native %v, the hooks' unread %+v %v", a.sessionNative("s-n"), page, err)
	}
	a.sess.claimMu.Lock()
	_, held := a.sess.claims[m.ID]
	a.sess.claimMu.Unlock()
	if held {
		t.Fatal("the dead module's claim still holds the message")
	}
	if s, err := a.RegisterSession(classic); err != nil || s.Native {
		t.Fatalf("the hooks' registration after the module fell silent: %+v %v", s, err)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n"}); !slices.Equal(got, []string{m.ID}) {
		t.Fatalf("the hooks' claim: %v", got)
	}
	// The module is back while the hooks deliver m: they stand down again, and
	// the module is not handed what they hold.
	if s, err := a.RegisterSession(native); err != nil || !s.Native {
		t.Fatalf("the module's registration: %+v %v", s, err)
	}
	m2 := ask(t, a, b, "after the module came back")
	if page, _ := a.UnreadQuery(nativeQuery(dir)); page.Total != 0 {
		t.Fatalf("the shell hooks got %+v from a native session", page)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID, m2.ID}, SessionID: "s-n"}); len(got) != 0 {
		t.Fatalf("the hooks' claim was granted: %v", got)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID, m2.ID}, SessionID: "s-n", Native: true, WakeToken: "tok12345"}); !slices.Equal(got, []string{m2.ID}) {
		t.Fatalf("the module woke with %v, want only the new message (the hooks deliver the other)", got)
	}
	// The hooks acknowledge what they delivered: read once.
	if res, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-n"}); err != nil || len(res) != 1 || !res[0].WasUnread {
		t.Fatalf("the hooks' ack: %+v %v", res, err)
	}
	// A heartbeat of the module counts as much as its registration.
	moduleSilent(a, nativeSeenTTL-time.Second)
	native.Heartbeat = true
	if _, err := a.RegisterSession(native); err != nil {
		t.Fatal(err)
	}
	a.sess.mu.Lock()
	seen := a.sess.sessions["s-n"].NativeSeen
	a.sess.mu.Unlock()
	if time.Since(seen) > time.Minute || !a.sessionNative("s-n") {
		t.Fatalf("the module's heartbeat did not count: seen %v", seen)
	}
}

// A wake whose prompt waits behind the person's running turn is kept alive by
// its module (the same token again): it does not lapse after inboxWakeGrace,
// so the running turn is not handed the message the queued prompt carries.
func TestWakeClaimRenewedBySameToken(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	m := ask(t, a, b, "woken, the prompt queued behind the person's turn")
	wake := ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true, WakeToken: "tok12345"}
	if got, _ := a.Claim(wake); len(got) != 1 {
		t.Fatal("wake claim")
	}
	turn := nativeQuery(dir, "native", "1")
	// Renewed again and again, well past the wake's grace and its wakes' count
	// (maxWaiterWakes): each time the same wake, held anew.
	for i := range 4 {
		a.sess.claimMu.Lock()
		c := a.sess.claims[m.ID]
		c.at = time.Now().Add(-inboxWakeGrace + 5*time.Second)
		a.sess.claims[m.ID] = c
		a.sess.claimMu.Unlock()
		if got, err := a.Claim(wake); err != nil || !slices.Equal(got, []string{m.ID}) {
			t.Fatalf("renewal %d: %v %v", i, got, err)
		}
		a.sess.claimMu.Lock()
		c = a.sess.claims[m.ID]
		a.sess.claimMu.Unlock()
		if time.Since(c.at) > time.Minute || c.token != "tok12345" || !c.wake() {
			t.Fatalf("renewal %d left the claim %+v", i, c)
		}
	}
	if v := leaseViewOf(t, a, m.ID); v.Attempts != 1 || time.Until(v.Deadline) < inboxWakeGrace-time.Minute {
		t.Fatalf("the renewals counted as deliveries, or the lease's deadline stayed: %+v", v)
	}
	page, _ := a.UnreadQuery(turn)
	got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true})
	if page.Total != 0 || len(page.Woken) != 1 || len(got) != 0 {
		t.Fatalf("a renewed wake is handed to the running turn too: unread %d, woken %d, claim %v", page.Total, len(page.Woken), got)
	}
	// Another wake (another token) does not take what this one holds.
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true, WakeToken: "tok99999"}); len(got) != 0 {
		t.Fatalf("a second wake took it: %v", got)
	}
	// Not renewed (the module dropped it): it lapses and the turn gets it, as before.
	lapse(a, m.ID)
	if page, _ := a.UnreadQuery(turn); page.Total != 1 {
		t.Fatalf("a lapsed wake stays hidden from the turn: %+v", page)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true}); len(got) != 1 {
		t.Fatalf("the turn does not get a lapsed wake's message: %v", got)
	}
}

// A held GET /unread asks again when the node says something changed, not on
// a short timer: an idle session costs one look per change, and one per
// unreadWaitTick.
func TestWaitUnreadWakesOnChange(t *testing.T) {
	oldTick, oldGap := unreadWaitTick, unreadWaitGap
	unreadWaitTick, unreadWaitGap = time.Hour, 20*time.Millisecond
	t.Cleanup(func() { unreadWaitTick, unreadWaitGap = oldTick, oldGap })
	var sig changeSignal
	var asked, has atomic.Int32
	done := make(chan UnreadPage, 1)
	go func() {
		page, _ := WaitUnread(t.Context(), url.Values{"wait": {"30"}}, func() (UnreadPage, error) {
			asked.Add(1)
			return UnreadPage{Total: int(has.Load())}, nil
		}, sig.wait)
		done <- page
	}()
	eventually(t, "the first look", func() bool { return asked.Load() == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := asked.Load(); n != 1 {
		t.Fatalf("an idle held request looked %d times", n)
	}
	sig.fire() // a change that brought nothing for this session
	eventually(t, "a look after the change", func() bool { return asked.Load() == 2 })
	has.Store(1)
	sig.fire()
	select {
	case page := <-done:
		if page.Total != 1 || asked.Load() != 3 {
			t.Fatalf("answered %+v after %d looks", page, asked.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held request did not hear the change")
	}
	// Without a signal it still looks every unreadWaitTick, and ends in time.
	unreadWaitTick = 30 * time.Millisecond
	asked.Store(0)
	has.Store(0)
	start := time.Now()
	if page, err := WaitUnread(t.Context(), url.Values{"wait": {"1"}}, func() (UnreadPage, error) {
		asked.Add(1)
		return UnreadPage{}, nil
	}); err != nil || page.Total != 0 || asked.Load() < 3 || time.Since(start) > 5*time.Second {
		t.Fatalf("the fallback tick: %+v %v, %d looks in %v", page, err, asked.Load(), time.Since(start))
	}
	// The client left while it waited.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	unreadWaitTick = time.Hour
	start = time.Now()
	if _, err := WaitUnread(ctx, url.Values{"wait": {"30"}}, func() (UnreadPage, error) { return UnreadPage{}, nil }, sig.wait); err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("a request whose client left: %v after %v", err, time.Since(start))
	}
}

// A seat's message is held by its seat's mark, not by a session claim: the
// wake that has it is renewed by its own token there too, past the wakes'
// count (maxWaiterWakes), and is no new wake that needs a person.
func TestSeatWakeRenewedBySameToken(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	_, codex := addSeats(t, a)
	if _, err := a.RegisterSession(SessionRequest{SessionID: codex.SessionID, Provider: codex.Provider, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	chat, _ := a.NewProjectChat(nil)
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "review this", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}})
	if err != nil {
		t.Fatal(err)
	}
	key := leaseKey(codex.ID, m.ID)
	wake := func(token string) []string {
		t.Helper()
		got, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: codex.SessionID, WakeToken: token})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Its two wakes: the first lapsed, the second holds it.
	if got := wake("tok11111"); !slices.Equal(got, []string{m.ID}) {
		t.Fatalf("the first wake: %v", got)
	}
	a.seats.mu.Lock()
	h := a.seats.marks[seatKey(codex.ID, m.ID)]
	h.at = time.Now().Add(-inboxWakeGrace - time.Second)
	a.seats.marks[seatKey(codex.ID, m.ID)] = h
	a.seats.mu.Unlock()
	a.leases.mu.Lock()
	a.leases.m[key].Deadline = time.Now().Add(-time.Second)
	a.leases.mu.Unlock()
	if got := wake("tok22222"); !slices.Equal(got, []string{m.ID}) {
		t.Fatalf("the second wake: %v", got)
	}
	if v, _ := a.leases.get(key); v.WaiterWakes != maxWaiterWakes {
		t.Fatalf("waiter wakes %d, want %d", v.WaiterWakes, maxWaiterWakes)
	}
	// Kept alive by its owner: the same wake, held anew, no person asked for.
	for i := range 3 {
		if got := wake("tok22222"); !slices.Equal(got, []string{m.ID}) {
			t.Fatalf("renewal %d of the seat's wake: %v", i, got)
		}
	}
	if v, _ := a.leases.get(key); v.Failed || v.State != LeaseLeased || v.Token != "tok22222" {
		t.Fatalf("the renewals changed the lease: %+v", v)
	}
	// A third wake is one too many, as for any message.
	if got := wake("tok33333"); len(got) != 0 {
		t.Fatalf("a third wake took it: %v", got)
	}
}

// The lapse of a silent module drops its claims before the session is its
// shell hooks' again: a hook's claim that follows the lapse is never among
// those dropped.
func TestNativeLapseKeepsTheHooksClaim(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	native := SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, Native: true}
	m := ask(t, a, b, "claimed by the module, then by the hooks")
	for i := range 300 {
		if _, err := a.RegisterSession(native); err != nil {
			t.Fatal(err)
		}
		if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true}); len(got) != 1 {
			t.Fatalf("round %d: the module's claim: %v", i, got)
		}
		moduleSilent(a, nativeSeenTTL+time.Second)
		var granted atomic.Int32
		done := make(chan struct{}, 4)
		for range 2 {
			go func() {
				a.sessionNative("s-n")
				done <- struct{}{}
			}()
			go func() {
				if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n"}); len(got) == 1 {
					granted.Add(1)
				}
				done <- struct{}{}
			}()
		}
		for range 4 {
			<-done
		}
		if granted.Load() == 0 {
			t.Fatalf("round %d: the hooks' claim after the lapse was not granted", i)
		}
		a.sess.claimMu.Lock()
		c, held := a.sess.claims[m.ID]
		a.sess.claimMu.Unlock()
		if !held || c.owner != "s-n" {
			t.Fatalf("round %d: the lapse dropped the claim the hooks took after it", i)
		}
		a.unclaim("s-n", []string{m.ID})
	}
}
