package node

import (
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fillQueue is a queue session (s-q) beside a Claude session of the same
// folder (s-c), which takes the messages named to taken exactly between the
// listing of s-q's fetch and its delivery (Node.fetchListed), once.
type fillQueue struct {
	*staleQueue
	taken []string
	state func() // run there, after s-c took them
}

func newFillQueue(t *testing.T) *fillQueue {
	t.Helper()
	q := &fillQueue{staleQueue: newStaleQueue(t)}
	q.rollout("")
	// Unpinned: a message for the folder is any live session's to take.
	if _, err := q.a.UnpinSession(PinSessionRequest{SessionID: "s-q"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.a.RegisterSession(SessionRequest{SessionID: "s-c", Provider: ProviderClaude, Folder: q.dir, Wake: WakeRewake}); err != nil {
		t.Fatal(err)
	}
	q.a.fetchListed = func() {
		ids := q.taken
		q.taken = nil
		if len(ids) > 0 {
			if granted, err := q.a.Claim(ClaimRequest{SessionID: "s-c", IDs: ids}); err != nil || !slices.Equal(granted, ids) {
				t.Errorf("the other session's claim of %v: %v %v", ids, granted, err)
			}
		}
		if q.state != nil {
			q.state()
		}
	}
	return q
}

// sized is a new request whose formatted text is about part/10 of one
// answer's budget.
func (q *fillQueue) sized(part int, name string) string {
	q.t.Helper()
	return q.send(strings.Repeat("я", FormatBudget*part/10) + " " + name).ID
}

// listed is the queue session's unread messages as a fetch lists them.
func (q *fillQueue) listed() []UnreadMessage {
	q.t.Helper()
	page, err := q.a.UnreadFor(q.dir, "s-q", "", 100)
	if err != nil {
		q.t.Fatal(err)
	}
	return page.Messages
}

// untouched fails unless id is unread, not read by the fetch, and s-c's.
func (q *fillQueue) untouched(id string) {
	q.t.Helper()
	all, err := q.a.Unread("", "", 100)
	if err != nil || !slices.Contains(ids(all.Messages), id) {
		q.t.Errorf("message %s, taken by the other session, is unread no more (%v)", id, err)
	}
	if rec, _ := q.a.chats.message(id); !rec.ReadAt.IsZero() {
		q.t.Errorf("message %s, taken by the other session, was acknowledged by the fetch", id)
	}
	if l, ok := q.a.leases.get(id); !ok || l.Owner != "s-c" {
		q.t.Errorf("message %s is not the other session's: %+v", id, l)
	}
}

// Issue #42. The listing is [large m1, m2] and one answer carries m1 alone;
// another owner takes m1 after the listing. The fetch returned nothing and
// said nothing (m2 then waited for pointerFetchGrace and a new wake): now the
// same call passes over m1 and delivers m2.
func TestFetchPassesOverWhatAnotherOwnerTook(t *testing.T) {
	q := newFillQueue(t)
	m1, m2 := q.sized(7, "m1"), q.sized(7, "m2")
	if l := q.listed(); !slices.Equal(ids(l), []string{m1, m2}) || FitUnread(l) != 1 {
		t.Fatalf("the listing: %v, of which %d fit one answer; want [m1 m2] and 1", ids(l), FitUnread(l))
	}
	q.taken = []string{m1}
	page := q.fetch()
	if !slices.Equal(ids(page.Messages), []string{m2}) || !page.Delivered || page.Note != "" || page.Next != "" {
		t.Fatalf("the fetch: %v delivered=%v note=%q next=%q; want [m2] delivered, no note", ids(page.Messages), page.Delivered, page.Note, page.Next)
	}
	q.untouched(m1)
	if rec, _ := q.a.chats.message(m2); rec.ReadAt.IsZero() {
		t.Fatal("the delivered message is not read")
	}
	if again := q.fetch(); again.Delivered || len(again.Messages) != 0 || again.Note != "" {
		t.Fatalf("the call after it: %v note=%q", ids(again.Messages), again.Note)
	}
}

// Nothing of the listing can be taken: an empty answer with no note (a call
// made again would be answered the same), and nothing changed by the fetch.
func TestFetchOfNothingClaimableIsEmpty(t *testing.T) {
	q := newFillQueue(t)
	m1, m2 := q.sized(7, "m1"), q.sized(7, "m2")
	var leases map[string]Lease
	var unread []string
	q.state = func() { leases, _, unread = q.state2() }
	q.taken = []string{m1, m2}
	page := q.fetch()
	if page.Delivered || len(page.Messages) != 0 || page.Note != "" || page.Next != "" {
		t.Fatalf("the fetch: %v delivered=%v note=%q; want an empty answer with no note", ids(page.Messages), page.Delivered, page.Note)
	}
	after, _, left := q.state2()
	if !reflect.DeepEqual(leases, after) || !slices.Equal(unread, left) {
		t.Fatalf("the fetch changed the state:\n%+v %v\nafter it:\n%+v %v", leases, unread, after, left)
	}
	q.untouched(m1)
	q.untouched(m2)
}

// state2 is staleQueue.state (fillQueue's field hides the method).
func (q *fillQueue) state2() (map[string]Lease, Session, []string) { return q.staleQueue.state() }

// A message passed over takes none of the answer's room: [m1, m2] fit one
// answer and m3 does not; with m1 another owner's, m3 fits beside m2.
func TestFetchPassedOverTakesNoBudget(t *testing.T) {
	q := newFillQueue(t)
	m1, m2, m3 := q.sized(4, "m1"), q.sized(4, "m2"), q.sized(4, "m3")
	l := q.listed()
	if len(l) != 3 || FitUnread(l) != 2 || FitUnread(l[1:]) != 2 {
		t.Fatalf("the listing: %v, %d fit, %d without the first; want 3, 2, 2", ids(l), FitUnread(l), FitUnread(l[1:]))
	}
	q.taken = []string{m1}
	page := q.fetch()
	if !slices.Equal(ids(page.Messages), []string{m2, m3}) || !page.Delivered || page.Note != "" {
		t.Fatalf("the fetch: %v note=%q; want [m2 m3] and no note", ids(page.Messages), page.Note)
	}
	if FitUnread(page.Messages) != len(page.Messages) {
		t.Fatal("the answer is over one answer's budget")
	}
	q.untouched(m1)
}

// Following the answers until none has a note, over a listing of several pages
// in which some messages are another owner's (the whole first page too):
// every claimable one is delivered once, in order, the others are untouched,
// and the last answer has no note.
func TestFetchFollowedOverAMix(t *testing.T) {
	q := newFillQueue(t)
	var m []string
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		m = append(m, q.send("request "+name).ID)
	}
	q.taken = []string{m[0], m[1], m[3]}
	var got []string
	for call := 1; ; call++ {
		page, err := q.a.UnreadQuery(url.Values{"agent": {"1"}, "session": {"s-q"}, "limit": {"2"}})
		if err != nil || len(page.Messages) > 2 || page.Next != "" || page.Delivered != (len(page.Messages) > 0) {
			t.Fatalf("call %d: %v delivered=%v next=%q %v", call, ids(page.Messages), page.Delivered, page.Next, err)
		}
		if call == 1 && !slices.Equal(ids(page.Messages), []string{m[2], m[4]}) {
			t.Fatalf("the first call: %v; want the first two claimable ones %v, from behind a page of taken ones", ids(page.Messages), []string{m[2], m[4]})
		}
		got = append(got, ids(page.Messages)...)
		if page.Note == "" {
			break
		}
		if len(page.Messages) == 0 {
			t.Fatalf("call %d: a note with nothing delivered: the caller would spin", call)
		}
		if call > 6 {
			t.Fatal("the fetch never ends")
		}
	}
	if want := []string{m[2], m[4], m[5]}; !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want each of %v once", got, want)
	}
	for _, id := range []string{m[0], m[1], m[3]} {
		q.untouched(id)
	}
}

// FetchFill offers every listed message once, whatever is granted, and asks
// for a later page only while the answer has room.
func TestFetchFillOffersEachOnce(t *testing.T) {
	msg := func(id string) UnreadMessage {
		m := UnreadMessage{Cursor: "c-" + id}
		m.ID, m.Body = id, strings.Repeat("я", FormatBudget*6/10)
		return m
	}
	pages := map[string]UnreadPage{
		"":     {Messages: []UnreadMessage{msg("m1"), msg("m2")}, Total: 5, Next: "c-m2"},
		"c-m2": {Messages: []UnreadMessage{msg("m3"), msg("m4")}, Next: "c-m4"},
		"c-m4": {Messages: []UnreadMessage{msg("m5")}},
	}
	for _, grant := range [][]string{nil, {"m1"}, {"m4"}, {"m2", "m5"}, {"m5"}, {"m1", "m2", "m3", "m4", "m5"}} {
		page := pages[""]
		offered := map[string]int{}
		rounds := 0
		err := FetchFill(&page, 2, func(batch []UnreadMessage) ([]string, int) {
			rounds++
			var granted []string
			for _, m := range batch {
				offered[m.ID]++
				if slices.Contains(grant, m.ID) {
					granted = append(granted, m.ID)
				}
			}
			return granted, 0
		}, func(after string) (UnreadPage, error) { return pages[after], nil })
		if err != nil || rounds > 6 {
			t.Fatalf("grant %v: %v after %d rounds", grant, err, rounds)
		}
		for id, n := range offered {
			if n != 1 {
				t.Errorf("grant %v: %s offered %d times", grant, id, n)
			}
		}
		// One answer carries one of these messages.
		want := grant
		if len(want) > 1 {
			want = want[:1]
		}
		if !slices.Equal(ids(page.Messages), want) && (len(want) > 0 || len(page.Messages) > 0) {
			t.Errorf("grant %v: the answer %v, want %v", grant, ids(page.Messages), want)
		}
		last := len(want) == 1 && offered["m5"] == 1 && want[0] == "m5"
		if note := page.Note != ""; note != (len(want) == 1 && !last) {
			t.Errorf("grant %v: note %q with %v delivered and m5 offered %d times", grant, page.Note, ids(page.Messages), offered["m5"])
		}
	}
}
