package worker

import (
	"context"
	"sync"
)

// Slots is the pool of job slots that every worker of the app shares, so all
// contexts together run at most MaxJobs agents (docs/plans/projects-v1.md
// §3.5). It starts closed: the app first lets every worker Reattach, which
// Holds a slot for each job still running from before (never blocking, even
// past the capacity), and only then Opens it, so no new job takes the slot of
// one that is reattaching. Acquire waits until the pool is open and a slot is
// free. Lowering the capacity never stops a running job; it only makes new
// ones wait.
type Slots struct {
	mu     sync.Mutex
	cap    int
	used   int
	open   bool
	change chan struct{} // closed and replaced on every change
}

// NewSlots returns a closed pool of capacity slots (at least one).
func NewSlots(capacity int) *Slots {
	return &Slots{cap: max(capacity, 1), change: make(chan struct{})}
}

// changedLocked wakes every waiter. The caller holds s.mu.
func (s *Slots) changedLocked() {
	close(s.change)
	s.change = make(chan struct{})
}

// Open lets Acquire hand out slots. Opening again changes nothing.
func (s *Slots) Open() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		s.open = true
		s.changedLocked()
	}
}

// SetCapacity changes the number of slots (at least one).
func (s *Slots) SetCapacity(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cap = max(n, 1)
	s.changedLocked()
}

// InUse is the number of slots taken now.
func (s *Slots) InUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// Hold takes a slot at once, open or not and even past the capacity: a job
// that is already running. release gives it back; calling it again does nothing.
func (s *Slots) Hold() (release func()) {
	s.mu.Lock()
	s.used++
	s.mu.Unlock()
	return s.releaser()
}

// Acquire takes a slot for a new job, waiting until the pool is open and a
// slot is free. ok is false when ctx ended first.
func (s *Slots) Acquire(ctx context.Context) (release func(), ok bool) {
	for {
		s.mu.Lock()
		if s.open && s.used < s.cap {
			s.used++
			s.mu.Unlock()
			return s.releaser(), true
		}
		change := s.change
		s.mu.Unlock()
		select {
		case <-change:
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (s *Slots) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.used--
			s.changedLocked()
			s.mu.Unlock()
		})
	}
}
