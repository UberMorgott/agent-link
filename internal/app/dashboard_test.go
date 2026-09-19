package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

func TestBuildParticipantsCountsRealMessagesOnly(t *testing.T) {
	status := Status{Node: "alice", Members: []node.MemberInfo{
		{Name: "alice", Self: true, Online: true},
		{Name: "bob", Online: true},
		{Name: "карл", Online: false, Seen: time.Unix(40, 0).UTC()},
	}}
	request := strings.Repeat("a", 32)
	entries := []node.Entry{
		entry("out", request, "alice", "bob", "q", at(10), job(node.JobRunning)),
		entry("in", strings.Repeat("b", 32), "bob", "alice", "a", at(20), replyTo(request)),
		entry("out", strings.Repeat("c", 32), "alice", "карл", "привет", at(30)),
		entry("in", strings.Repeat("d", 32), "карл", "alice", "ответ", at(40)),
		func() node.Entry {
			e := entry("in", strings.Repeat("e", 32), "bob", "alice", "", at(15), replyTo(request))
			e.Kind = node.KindStatus
			return e
		}(),
	}

	got := buildParticipants(status, entries)
	if len(got) != 2 {
		t.Fatalf("got %d participants, want 2: %+v", len(got), got)
	}
	if got[0].Name != "bob" || got[0].Sent != 1 || got[0].Received != 1 || got[0].Total != 2 ||
		!got[0].LatestAt.Equal(time.Unix(20, 0).UTC()) || got[0].LatestPreview != "a" || got[0].LatestDirection != "in" {
		t.Fatalf("bob: %+v", got[0])
	}
	if got[1].Name != "карл" || got[1].Sent != 1 || got[1].Received != 1 || got[1].Total != 2 ||
		!got[1].LatestAt.Equal(time.Unix(40, 0).UTC()) || got[1].LatestPreview != "ответ" || got[1].LatestDirection != "in" {
		t.Fatalf("карл: %+v", got[1])
	}
}

func TestBuildParticipantsCarriesLatestDirectionBeyondDashboardRecentLimit(t *testing.T) {
	status := Status{Node: "alice", Members: []node.MemberInfo{{Name: "alice", Self: true}}}
	entries := make([]node.Entry, 0, 6)
	for i := range 6 {
		peer := fmt.Sprintf("peer-%d", i)
		status.Members = append(status.Members, node.MemberInfo{Name: peer})
		direction, from, to := "out", "alice", peer
		if i == 0 {
			direction, from, to = "in", peer, "alice"
		}
		entries = append(entries, entry(direction, fmt.Sprintf("%032x", i+1), from, to, "latest "+peer, at(int64(i+1))))
	}

	participants := buildParticipants(status, entries)
	if len(buildDashboard(status, entries).Recent) != 5 {
		t.Fatal("fixture must put one participant outside the dashboard recent cap")
	}
	if participants[0].Name != "peer-0" || participants[0].LatestDirection != "in" || participants[0].LatestPreview != "latest peer-0" {
		t.Fatalf("oldest participant lost unread basis: %+v", participants[0])
	}
}

func TestBuildParticipantsLatestTieUsesMessageIDIndependentOfInputOrder(t *testing.T) {
	status := Status{Node: "alice", Members: []node.MemberInfo{{Name: "alice", Self: true}, {Name: "bob"}}}
	statusEntry := func() node.Entry {
		e := entry("in", strings.Repeat("f", 32), "bob", "alice", "ignored status", at(10))
		e.Kind = node.KindStatus
		return e
	}
	outbound := entry("out", strings.Repeat("a", 32), "alice", "bob", "outbound", at(10))
	inbound := entry("in", strings.Repeat("b", 32), "bob", "alice", "inbound", at(10))
	cases := []struct {
		name    string
		entries []node.Entry
	}{
		{name: "out then in", entries: []node.Entry{outbound, inbound, statusEntry()}},
		{name: "in then out", entries: []node.Entry{inbound, outbound, statusEntry()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildParticipants(status, tc.entries)
			if len(got) != 1 || got[0].LatestDirection != "in" || got[0].LatestPreview != "inbound" ||
				!got[0].LatestAt.Equal(time.Unix(10, 0).UTC()) || got[0].Sent != 1 || got[0].Received != 1 || got[0].Total != 2 {
				t.Fatalf("participant tie: %+v", got)
			}
		})
	}
}

func TestBuildDashboardAggregatesActivityAndRecentConversations(t *testing.T) {
	request := strings.Repeat("a", 32)
	entries := []node.Entry{
		entry("out", request, "alice", "bob", "q", at(10), job(node.JobRunning)),
		entry("in", strings.Repeat("b", 32), "bob", "alice", "a", at(20), replyTo(request)),
		entry("out", strings.Repeat("c", 32), "alice", "карл", "привет", at(30)),
		entry("in", strings.Repeat("d", 32), "карл", "alice", "ответ", at(40)),
		func() node.Entry {
			e := entry("in", strings.Repeat("e", 32), "bob", "alice", "", at(15), replyTo(request))
			e.Kind = node.KindStatus
			return e
		}(),
	}

	got := buildDashboard(Status{Node: "alice"}, entries)
	if got.SentMessages != 2 || got.ReceivedMessages != 2 || got.TotalMessages != 4 || got.ActiveRequests != 1 {
		t.Fatalf("counts: %+v", got)
	}
	if len(got.Recent) != 2 || got.Recent[0].Peer != "карл" || got.Recent[0].Preview != "ответ" ||
		!got.Recent[0].LatestAt.Equal(time.Unix(40, 0).UTC()) || got.Recent[0].Direction != "in" ||
		got.Recent[1].Peer != "bob" || got.Recent[1].Preview != "a" ||
		!got.Recent[1].LatestAt.Equal(time.Unix(20, 0).UTC()) || got.Recent[1].Direction != "in" {
		t.Fatalf("recent: %+v", got.Recent)
	}
}

func TestBuildDashboardKeepsAllHistoryBeyondRecentPage(t *testing.T) {
	entries := make([]node.Entry, 205)
	for i := range entries {
		entries[i] = entry("out", fmt.Sprintf("%032x", i+1), "alice", "bob", fmt.Sprintf("m%d", i), at(int64(i)))
	}

	got := buildDashboard(Status{Node: "alice"}, entries)
	if got.TotalMessages != 205 || got.SentMessages != 205 || len(got.Recent) != 1 {
		t.Fatalf("summary: %+v", got)
	}
}

func TestThreadPeerUsesRemoteParticipant(t *testing.T) {
	cases := []struct {
		name, local, want string
		thread            Thread
	}{
		{name: "inbound", local: "alice", thread: Thread{Direction: "in", From: "bob", To: "alice"}, want: "bob"},
		{name: "outbound", local: "alice", thread: Thread{Direction: "out", From: "alice", To: "bob"}, want: "bob"},
		{name: "outbound after local rename", local: "alice-new", thread: Thread{Direction: "out", From: "alice-old", To: "bob"}, want: "bob"},
		{name: "reply without its request", local: "alice", thread: Thread{Direction: "in", From: "bob", To: "alice", Answer: "answer"}, want: "bob"},
		{name: "area delivery", local: "alice", thread: Thread{Direction: "out", From: "alice", To: "bob", Area: "dev"}, want: "bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := threadPeer(tc.thread, tc.local); got != tc.want {
				t.Fatalf("threadPeer(%+v, %q) = %q, want %q", tc.thread, tc.local, got, tc.want)
			}
		})
	}
}

func TestFilterThreadsMatchesExactPeerAndKeepsEmptyFilter(t *testing.T) {
	threads := []Thread{
		{Direction: "in", From: "карл & sons", To: "alice"},
		{Direction: "out", From: "alice", To: "bob"},
		{Direction: "out", From: "alice", To: "карл & sons"},
	}

	got := filterThreads(threads, "карл & sons", "alice")
	if len(got) != 2 || threadPeer(got[0], "alice") != "карл & sons" || threadPeer(got[1], "alice") != "карл & sons" {
		t.Fatalf("filtered: %+v", got)
	}
	got = filterThreads(threads, "", "alice")
	if len(got) != 3 {
		t.Fatalf("empty filter: got %d, want 3", len(got))
	}
}
