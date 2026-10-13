package node

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// The turn of a Codex thread, read from its rollout: the busy signal of a
// WakeQueue session that does not depend on its hooks. A thread whose hooks
// deliver no event stays Idle for the node while it is inside a long turn;
// every prompt queued then waits behind that turn, one per new message.
//
// A rollout is JSON lines, and a turn is bracketed by event records (read
// from real rollouts, codex-cli 0.159 and older ones without "ordinal"):
//
//	{"timestamp":"…Z",["ordinal":N,]"type":"event_msg","payload":{"type":"task_started",…}}
//	{"timestamp":"…Z",["ordinal":N,]"type":"event_msg","payload":{"type":"task_complete",…}}
//	{"timestamp":"…Z",["ordinal":N,]"type":"event_msg","payload":{"type":"turn_aborted",…}}
//
// The last of them says whether a turn runs. A thread killed inside a turn
// writes no end, so a running turn counts only while its rollout was written
// to within rolloutTurnQuiet. Everything else (no rollout, an unreadable one,
// no turn record, a record without a time) is no signal: the session is woken
// as without this check.

// Turn states of a rollout (rolloutTurn.state).
const (
	turnNone    = iota // no turn record read
	turnRunning        // the last one is a start
	turnEnded          // the last one is a completion or an abort
)

// rolloutTurn is the last turn record of a rollout: its kind, its own time,
// and when the rollout was last written to.
type rolloutTurn struct {
	state int
	at    time.Time
	mod   time.Time
}

// turnRecord is the mark of an event record, before its payload's type; it
// stands within turnRecordWithin bytes of the line's start. Inside a JSON
// string its quotes would be escaped, so no message text reads as one.
const (
	turnRecord       = `"type":"event_msg","payload":{"type":"`
	turnRecordWithin = 128
	turnStamp        = `{"timestamp":"`
	// rolloutHead is how much of a line is kept: a record's start. Lines run
	// to megabytes (tool output, images).
	rolloutHead = 256
)

// turnOf reads the head of one rollout line: the turn state it sets and its
// time; turnNone for every other record.
func turnOf(head []byte) (int, time.Time) {
	i := bytes.Index(head, []byte(turnRecord))
	if i < 0 || i > turnRecordWithin {
		return turnNone, time.Time{}
	}
	name, _, ok := bytes.Cut(head[i+len(turnRecord):], []byte{'"'})
	if !ok {
		return turnNone, time.Time{}
	}
	var state int
	switch string(name) {
	case "task_started":
		state = turnRunning
	case "task_complete", "turn_aborted":
		state = turnEnded
	default:
		return turnNone, time.Time{}
	}
	var at time.Time
	if rest, ok := bytes.CutPrefix(head, []byte(turnStamp)); ok {
		if stamp, _, ok := bytes.Cut(rest, []byte{'"'}); ok {
			at, _ = time.Parse(time.RFC3339Nano, string(stamp))
		}
	}
	if at.IsZero() {
		return turnNone, time.Time{} // no time to compare with the hooks': no signal
	}
	return state, at
}

// scanTurns reads the complete lines of r, which starts at offset off of a
// rollout whose turn so far is turn, and returns the offset after the last
// complete line and the turn then. A line still being written is left for the
// next read.
func scanTurns(r io.Reader, off int64, turn rolloutTurn) (int64, rolloutTurn) {
	br := bufio.NewReaderSize(r, 64<<10)
	head := make([]byte, 0, rolloutHead)
	for {
		head = head[:0]
		var n int64
		for first := true; ; first = false {
			frag, err := br.ReadSlice('\n')
			n += int64(len(frag))
			if first {
				head = append(head, frag[:min(len(frag), rolloutHead)]...)
			}
			if err == nil {
				break
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				return off, turn // the end, or an error: what was read counts
			}
		}
		off += n
		if state, at := turnOf(head); state != turnNone {
			turn.state, turn.at = state, at
		}
	}
}

// rolloutLookupEvery is how long a thread's rollout path is kept before it is
// looked up again (codexRollout walks every day's folder).
const rolloutLookupEvery = time.Minute

// rolloutBookMax bounds the threads a rolloutBook remembers.
const rolloutBookMax = 256

// rolloutBook reads the turns of Codex threads from their rollouts, each
// rollout once: a later read continues where the last one ended.
type rolloutBook struct {
	mu sync.Mutex
	m  map[string]*rolloutSeen
}

type rolloutSeen struct {
	path  string
	found time.Time // when path was looked up
	off   int64     // read up to here
	turn  rolloutTurn
}

// turn is the turn of thread sid of the Codex home home; false when its
// rollout is missing or cannot be read.
func (b *rolloutBook) turn(home, sid string, now time.Time) (rolloutTurn, bool) {
	if home == "" || !validSessionID(sid) {
		return rolloutTurn{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := home + "\x00" + sid
	e := b.m[key]
	if e == nil || now.Sub(e.found) >= rolloutLookupEvery || now.Before(e.found) {
		path := codexRollout(home, sid)
		if path == "" {
			delete(b.m, key)
			return rolloutTurn{}, false
		}
		if e == nil || e.path != path {
			if b.m == nil || len(b.m) >= rolloutBookMax {
				b.m = map[string]*rolloutSeen{}
			}
			e = &rolloutSeen{path: path}
			b.m[key] = e
		}
		e.found = now
	}
	f, err := os.Open(e.path)
	if err != nil {
		delete(b.m, key)
		return rolloutTurn{}, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		delete(b.m, key)
		return rolloutTurn{}, false
	}
	if info.Size() < e.off {
		e.off, e.turn = 0, rolloutTurn{} // not the file that was read
	}
	if info.Size() > e.off {
		if _, err := f.Seek(e.off, io.SeekStart); err != nil {
			delete(b.m, key)
			return rolloutTurn{}, false
		}
		e.off, e.turn = scanTurns(f, e.off, e.turn)
	}
	e.turn.mod = info.ModTime()
	return e.turn, true
}

// queueHeld reports whether no prompt may be queued in WakeQueue session s
// now, by its thread's rollout:
//
//   - it is in a turn the hooks did not report: the turn started after the
//     last hook event that said idle (Session.IdleAt; a fresh hook wins) and
//     its rollout was written to within rolloutTurnQuiet. Its messages wait
//     for the turn's end, and one wake then carries them all;
//   - a prompt of the node is still in its queue (Woken, unproven) and a turn
//     ended after it was queued: the thread runs that prompt next, so the
//     waits before another wake (wakeDue) count from that end, not from the
//     wake. A thread that stays silent after it gets the stale wakes as ever.
//
// No rollout, or nothing in it that says so: false (the session is woken).
func (n *Node) queueHeld(s Session, now time.Time) bool {
	home := s.CodexHome
	if home == "" {
		home = agentHome("CODEX_HOME", ".codex")
	}
	t, ok := n.rollouts.turn(home, s.SessionID, now)
	if !ok {
		return false
	}
	switch t.state {
	case turnRunning:
		idleAt := s.IdleAt
		if idleAt.IsZero() {
			idleAt = s.LastActive // a record from before IdleAt
		}
		return t.at.After(idleAt) && now.Sub(t.mod) < rolloutTurnQuiet
	case turnEnded:
		if s.Woken && t.at.After(s.WokeAt) {
			s.WokeAt = t.at
			return !s.wakeDue(now)
		}
	}
	return false
}
