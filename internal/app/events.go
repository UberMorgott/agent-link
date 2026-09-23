package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// changeEvent is the data of an SSE "change" event. Topics name what changed:
// "chats" (a chat was created, closed or archived: reload the chat list),
// "messages" (a message, delivery or job status changed: reload the open chat
// and the list), "worker", "peer", "members", "settings", "status",
// "dashboard", "participants", "update"; "all" after (re)connecting.
type changeEvent struct {
	Revision uint64   `json:"revision"`
	Topics   []string `json:"topics"`
}

// eventBroadcaster turns state changes into edge-triggered subscriber wakes.
// A slow subscriber stores no event queue: it reads the latest revision and
// derives the union of topics changed since its own cursor.
type eventBroadcaster struct {
	mu          sync.Mutex
	revision    uint64
	topics      map[string]uint64
	subscribers map[*eventSubscription]struct{}
}

type eventSubscription struct {
	b       *eventBroadcaster
	wake    chan struct{}
	cursor  uint64
	initial bool
	once    sync.Once
}

func newEventBroadcaster() *eventBroadcaster {
	return &eventBroadcaster{topics: map[string]uint64{}, subscribers: map[*eventSubscription]struct{}{}}
}

func (b *eventBroadcaster) publish(topics ...string) {
	if b == nil || len(topics) == 0 {
		return
	}
	b.mu.Lock()
	b.revision++
	for _, topic := range topics {
		if topic != "" {
			b.topics[topic] = b.revision
		}
	}
	for s := range b.subscribers {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

func (b *eventBroadcaster) subscribe() *eventSubscription {
	s := &eventSubscription{b: b, wake: make(chan struct{}, 1), initial: true}
	b.mu.Lock()
	s.cursor = b.revision
	b.subscribers[s] = struct{}{}
	b.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s
}

func (s *eventSubscription) next() changeEvent {
	<-s.wake
	return s.snapshot()
}

func (s *eventSubscription) snapshot() changeEvent {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.initial {
		s.initial = false
		s.cursor = s.b.revision
		return changeEvent{Revision: s.cursor, Topics: []string{"all"}}
	}
	if s.cursor == s.b.revision {
		return changeEvent{Revision: s.cursor, Topics: []string{"all"}}
	}
	e := changeEvent{Revision: s.b.revision}
	for topic, revision := range s.b.topics {
		if revision > s.cursor {
			e.Topics = append(e.Topics, topic)
		}
	}
	slices.Sort(e.Topics)
	s.cursor = e.Revision
	return e
}

func (s *eventSubscription) close() {
	s.once.Do(func() {
		s.b.mu.Lock()
		delete(s.b.subscribers, s)
		s.b.mu.Unlock()
	})
}

func (a *App) eventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	s := a.events.subscribe()
	defer s.close()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.wake:
			data, err := json.Marshal(s.snapshot())
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: change\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
