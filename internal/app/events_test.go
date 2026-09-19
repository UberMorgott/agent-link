package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReactiveEventsInitialAndCoalescedRevision(t *testing.T) {
	b := newEventBroadcaster()
	s := b.subscribe()
	defer s.close()

	b.publish("status") // a change racing the first flush is covered by "all"
	initial := s.next()
	if initial.Revision != 1 || len(initial.Topics) != 1 || initial.Topics[0] != "all" {
		t.Fatalf("initial event = %+v", initial)
	}
	b.publish("status")
	b.publish("messages")
	b.publish("status")
	got := s.next()
	if got.Revision != 4 || strings.Join(got.Topics, ",") != "messages,status" {
		t.Fatalf("coalesced event = %+v", got)
	}
}

func TestReactiveEventsUnsubscribe(t *testing.T) {
	b := newEventBroadcaster()
	s := b.subscribe()
	if len(b.subscribers) != 1 {
		t.Fatalf("subscribers = %d", len(b.subscribers))
	}
	s.close()
	if len(b.subscribers) != 0 {
		t.Fatalf("subscriber leaked after close: %d", len(b.subscribers))
	}
	b.publish("status") // must not block or panic after cancellation
}

func TestReactiveEventsEndpointRequiresTokenAndStreamsChanges(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(t, http.MethodGet, "/ui/api/events", "", nil); code != http.StatusForbidden {
		t.Fatalf("events without token = %d, want 403", code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/ui/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TokenHeader, h.app.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events response = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	read := make(chan changeEvent, 2)
	go func() {
		dec := json.NewDecoder(newSSEDataReader(resp.Body))
		for range 2 {
			var event changeEvent
			if dec.Decode(&event) != nil {
				return
			}
			read <- event
		}
	}()
	select {
	case event := <-read:
		if event.Revision != 0 || strings.Join(event.Topics, ",") != "all" {
			t.Fatalf("initial stream event = %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initial event was not flushed")
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("settings mutation: %d %s", code, body)
	}
	select {
	case event := <-read:
		if event.Revision == 0 || !slices.Contains(event.Topics, "settings") || !slices.Contains(event.Topics, "status") {
			t.Fatalf("change stream event = %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("change event was not flushed")
	}
	cancel()
	eventuallyApp(t, "event stream unsubscribe", func() bool {
		h.app.events.mu.Lock()
		defer h.app.events.mu.Unlock()
		return len(h.app.events.subscribers) == 0
	})
}

// newSSEDataReader extracts JSON data lines from the stream for the endpoint test.
func newSSEDataReader(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		buf := make([]byte, 1)
		var line strings.Builder
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if buf[0] == '\n' {
					s := strings.TrimSuffix(line.String(), "\r")
					line.Reset()
					if data, ok := strings.CutPrefix(s, "data: "); ok {
						_, _ = io.WriteString(pw, data+"\n")
					}
				} else {
					line.WriteByte(buf[0])
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return pr
}

func eventuallyApp(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
