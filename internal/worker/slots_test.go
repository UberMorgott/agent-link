package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// blockingRun counts running jobs (and the most at once) and holds each until release closes.
type blockingRun struct {
	running, most atomic.Int32
	release       chan struct{}
}

func newBlockingRun() *blockingRun { return &blockingRun{release: make(chan struct{})} }

func (b *blockingRun) run(ctx context.Context, _, prompt string, _ func(string)) (string, error) {
	n := b.running.Add(1)
	for {
		m := b.most.Load()
		if n <= m || b.most.CompareAndSwap(m, n) {
			break
		}
	}
	defer b.running.Add(-1)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return prompt, nil
}

func sharedWorker(t *testing.T, run Runner, rec *recorder, slots *Slots, state string) *Worker {
	t.Helper()
	w, err := New(run, rec.send, state, t.TempDir(), Options{MaxJobs: 2, Slots: slots}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Two workers on one pool of one slot never run two jobs at once.
func TestSharedSlotsCapTwoWorkers(t *testing.T) {
	slots := NewSlots(1)
	slots.Open()
	b := newBlockingRun()
	rec := newRecorder()
	w1 := sharedWorker(t, b.run, rec, slots, t.TempDir())
	w2 := sharedWorker(t, b.run, rec, slots, t.TempDir())
	start(t, w1)
	start(t, w2)
	accept(t, w1, msg(id1, "one"))
	accept(t, w2, msg(id2, "two"))
	eventually(t, "one job running", func() bool { return b.running.Load() == 1 })
	time.Sleep(200 * time.Millisecond) // room for a second job if the cap were broken
	if n := b.running.Load(); n != 1 || slots.InUse() != 1 {
		t.Fatalf("running %d, slots in use %d", n, slots.InUse())
	}
	close(b.release)
	rec.wait(t, 2)
	if b.most.Load() != 1 {
		t.Fatalf("most at once %d, want 1", b.most.Load())
	}
	eventually(t, "slots free", func() bool { return slots.InUse() == 0 })
}

// No new job starts before the pool opens, even with a worker running; a
// reattached job holds its slot at once, past the capacity if need be.
func TestSharedSlotsOpenAfterReattach(t *testing.T) {
	slots := NewSlots(1)
	b := newBlockingRun()
	rec := newRecorder()
	w := sharedWorker(t, b.run, rec, slots, t.TempDir())
	accept(t, w, msg(id1, "early"))
	start(t, w)
	time.Sleep(300 * time.Millisecond)
	if b.running.Load() != 0 {
		t.Fatal("a job started before the slots opened")
	}
	// A late worker's reattached job: Hold never blocks, closed or full.
	held := slots.Hold()
	slots.Open()
	time.Sleep(200 * time.Millisecond)
	if b.running.Load() != 0 {
		t.Fatal("a new job took the slot of a reattached one")
	}
	held()
	held() // a second release does nothing
	eventually(t, "the job starts once the slot is free", func() bool { return b.running.Load() == 1 })
	if slots.InUse() != 1 {
		t.Fatalf("in use %d", slots.InUse())
	}
	close(b.release)
	rec.wait(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	hold := slots.Hold()
	cancel()
	if _, ok := slots.Acquire(ctx); ok {
		t.Fatal("acquired a full pool")
	}
	hold()
	slots.SetCapacity(0) // at least one
	if release, ok := slots.Acquire(context.Background()); !ok {
		t.Fatal("capacity below one")
	} else {
		release()
	}
}

// Reattach holds a slot for a detached job left running and Run releases the
// slots of reattached jobs it did not get to.
func TestReattachHoldsSlot(t *testing.T) {
	state := t.TempDir()
	slots := NewSlots(1)
	rec := newRecorder()
	w := sharedWorker(t, nil, rec, slots, state)
	w.opt.Agent = func() Command { return Command{Name: "missing-agent"} }
	w.jobs[id1] = &Job{Request: msg(id1, "x"), Seq: 1, Status: node.JobRunning, Attempts: 1, Proc: &Proc{PID: 1}}
	w.Reattach(t.Context())
	w.Reattach(t.Context()) // only the first call acts
	if slots.InUse() != 1 {
		t.Fatalf("in use %d after reattach, want 1", slots.InUse())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	if slots.InUse() != 0 {
		t.Fatalf("in use %d after Run, want 0", slots.InUse())
	}
}

// Quiesce refuses while any job is unfinished and leaves the intake open;
// otherwise it closes the intake at once (a request is not accepted, so not
// ACKed) until Resume.
func TestQuiesce(t *testing.T) {
	b := newBlockingRun()
	rec := newRecorder()
	w := newWorker(t, b.run, rec, t.TempDir(), 0)
	start(t, w)
	accept(t, w, msg(id1, "busy"))
	eventually(t, "running", func() bool { return b.running.Load() == 1 })
	if err := w.Quiesce(); !errors.Is(err, ErrBusy) || !w.Busy() {
		t.Fatalf("quiesce while busy: %v", err)
	}
	accept(t, w, msg(id2, "still taken"))
	close(b.release)
	rec.wait(t, 2)
	eventually(t, "idle", func() bool { return !w.Busy() })
	if err := w.Quiesce(); err != nil {
		t.Fatal(err)
	}
	if err := w.Accept(msg(id3, "late")); !errors.Is(err, errDraining) {
		t.Fatalf("accept while draining: %v", err)
	}
	if _, ok := w.Job(id3); ok {
		t.Fatal("a job queued while draining")
	}
	reply := msg("00000000000000000000000000000009", "an answer")
	reply.ReplyTo = id1
	if err := w.Accept(reply); err != nil {
		t.Fatalf("a reply while draining: %v", err)
	}
	w.Resume()
	accept(t, w, msg(id3, "after resume"))
	rec.wait(t, 1)
}

// Many workers racing on one pool never exceed it.
func TestSharedSlotsRace(t *testing.T) {
	slots := NewSlots(2)
	slots.Open()
	var inUse, most atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			release, ok := slots.Acquire(t.Context())
			if !ok {
				return
			}
			n := inUse.Add(1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(time.Millisecond)
			inUse.Add(-1)
			release()
		})
	}
	wg.Wait()
	if most.Load() > 2 {
		t.Fatalf("%d at once, want at most 2", most.Load())
	}
}
