package node

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Occupancy: a session can be open in a folder without being registered yet
// (a desktop app's session registers at its first hook, and one left open
// never registers again until it gets a prompt). Before it opens a session the
// launch ladder asks the agents' own records whether one was active in the
// folder within occupiedWithin, and opens none then (a second session would
// duplicate the work):
//
//   - Claude Code: a transcript (*.jsonl) of
//     <claude home>/projects/<encoded folder>/ written to within it. The folder
//     is encoded as Claude Code does: every character but an ASCII letter or
//     digit becomes '-' (E:\DEV\agent-link -> E--DEV-agent-link; checked on
//     Windows). A folder long enough for Claude Code to shorten its name
//     (about 200 characters) is not found: then the Codex check alone counts.
//   - Codex: a rollout (<codex home>/sessions/YYYY/MM/DD/*.jsonl, today's and
//     yesterday's) written to within it whose session_meta cwd is the folder
//     or inside it. Archived threads are moved out of sessions/, so they do not
//     count. Codex's thread database (state_5.sqlite) is not read: it needs a
//     SQLite driver this module does not carry; the rollouts hold the same
//     cwd and activity.
const occupiedWithin = 10 * time.Minute

// folderOccupied reports whether an agent session was active in dir lately
// (see above); n.occupied replaces the check in tests.
func (n *Node) folderOccupied(dir string, now time.Time) bool {
	if n.occupied != nil {
		return n.occupied(dir, now)
	}
	return agentOccupied(agentHome("CLAUDE_CONFIG_DIR", ".claude"), agentHome("CODEX_HOME", ".codex"), dir, now)
}

// seatOccupied reports whether seat s's session is open in the agent's app:
// its transcript (see above, for that session alone) was written after the
// node's last turn of it (Seat.LastTurn) and within occupiedWithin. The node
// does not resume it then (two writers of one session). n.seatBusy replaces
// the check in tests.
func (n *Node) seatOccupied(s Seat, now time.Time) bool {
	if n.seatBusy != nil {
		return n.seatBusy(s, now)
	}
	return sessionOccupied(s.Provider, agentHome("CLAUDE_CONFIG_DIR", ".claude"), agentHome("CODEX_HOME", ".codex"),
		n.folders.work, s.SessionID, s.LastTurn, now)
}

// sessionOccupied is the check of seatOccupied with explicit agent homes: a
// Claude Code transcript <claude home>/projects/<encoded dir>/<sid>.jsonl, a
// Codex rollout <codex home>/sessions/YYYY/MM/DD/rollout-*-<sid>.jsonl (of
// any day: a resumed thread writes to its first day's rollout).
func sessionOccupied(provider, claudeHome, codexHome, dir, sid string, since, now time.Time) bool {
	if !validSessionID(sid) {
		return false
	}
	var paths []string
	switch provider {
	case ProviderClaude:
		if claudeHome != "" && dir != "" {
			paths = []string{filepath.Join(claudeHome, "projects", claudeProjectDir(filepath.Clean(dir)), sid+".jsonl")}
		}
	case ProviderCodex:
		if codexHome != "" {
			if path := codexRollout(codexHome, sid); path != "" {
				paths = []string{path}
			}
		}
	}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && recentFile(info, now) && info.ModTime().After(since) {
			return true
		}
	}
	return false
}

// codexRollout is the newest rollout of sid. A resumed thread keeps writing
// its first day's file, so the date cannot be inferred from the current day.
func codexRollout(home, sid string) string {
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*"+sid+".jsonl"))
	var found string
	var newest time.Time
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && (found == "" || info.ModTime().After(newest)) {
			found, newest = path, info.ModTime()
		}
	}
	return found
}

func rolloutContains(path, marker string) bool {
	const tail = int64(1024 * 1024)
	f, err := os.Open(path) //nolint:gosec // rollout under the configured Codex home
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		_, _ = f.Seek(-tail, io.SeekEnd)
	}
	b, err := io.ReadAll(io.LimitReader(f, tail))
	return err == nil && bytes.Contains(b, []byte(marker))
}

// agentHome is the agent's home: env when set, else ~/<dir>; "" when unknown.
func agentHome(env, dir string) string {
	if d := os.Getenv(env); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, dir)
}

// agentOccupied is the occupancy check with explicit agent homes.
func agentOccupied(claudeHome, codexHome, dir string, now time.Time) bool {
	return claudeOccupied(claudeHome, dir, now) || codexOccupied(codexHome, dir, now)
}

// claudeProjectDir is folder's directory name under Claude Code's projects/.
func claudeProjectDir(folder string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, folder)
}

func recentFile(info os.FileInfo, now time.Time) bool {
	return now.Sub(info.ModTime()) < occupiedWithin
}

func claudeOccupied(home, dir string, now time.Time) bool {
	if home == "" || dir == "" {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(home, "projects", claudeProjectDir(filepath.Clean(dir))))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info, err := e.Info(); err == nil && recentFile(info, now) {
			return true
		}
	}
	return false
}

func codexOccupied(home, dir string, now time.Time) bool {
	if home == "" || dir == "" {
		return false
	}
	for _, day := range []time.Time{now, now.Add(-24 * time.Hour)} {
		day = day.Local()
		sub := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || !recentFile(info, now) {
				continue
			}
			if cwd := rolloutCwd(filepath.Join(sub, e.Name())); cwd != "" && inFolder(dir, cwd) {
				return true
			}
		}
	}
	return false
}

// rolloutCwd is the cwd of a Codex rollout's first record (session_meta).
func rolloutCwd(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: a rollout under the Codex home
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var first struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.NewDecoder(f).Decode(&first) != nil || first.Type != "session_meta" {
		return ""
	}
	return first.Payload.Cwd
}
