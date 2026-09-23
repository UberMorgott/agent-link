package node

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ProjectMeta is what the members of a project share about it: its name,
// renamed by anyone, last writer wins (docs/plans/projects-v1.md §3.1). It
// travels only inside the project's own sealed sessions.
type ProjectMeta struct {
	ID        string    `json:"id"`      // project id
	Name      string    `json:"name"`    // 1..80 runes after trim, no control chars
	Lamport   uint64    `json:"lamport"` // LWW clock of Name
	Writer    string    `json:"writer"`  // node id of the last renamer (tie-break)
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// frameProject carries ProjectMeta (frame.ProjectMeta) to CapProjects peers:
// after members on every new session, and whenever a merge or rename changed it.
const frameProject = "project"

// maxProjectName bounds a project name, in runes.
const maxProjectName = 80

// projectMetaFile holds a project node's ProjectMeta in its data directory.
const projectMetaFile = "project.json"

// ErrProjectName: a project name is empty, too long or has control characters.
var ErrProjectName = errors.New("invalid project name")

// ErrNotProject: the operation needs a project context, not the legacy network.
var ErrNotProject = errors.New("not a project context")

// NormalizeProjectName trims name and checks it: 1..80 runes, no control characters.
func NormalizeProjectName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxProjectName ||
		strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrProjectName
	}
	return name, nil
}

// metaNewer reports whether r replaces l: (Lamport, Writer) compared
// lexicographically, higher wins.
func metaNewer(r, l ProjectMeta) bool {
	if r.Lamport != l.Lamport {
		return r.Lamport > l.Lamport
	}
	return r.Writer > l.Writer
}

// loadProjectMeta reads project.json; a missing file is a joiner's meta
// (Lamport 0, no name), which any received meta replaces.
func (n *Node) loadProjectMeta() error {
	n.meta = ProjectMeta{ID: n.cfg.Project}
	var m ProjectMeta
	err := readJSON(filepath.Join(n.cfg.DataDir, projectMetaFile), &m)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case m.ID != n.cfg.Project:
		return fmt.Errorf("%s names project %q, not %q", projectMetaFile, m.ID, n.cfg.Project)
	}
	n.meta = m
	return nil
}

// ProjectMeta returns the project's shared meta; Lamport 0 (and no name)
// until the creator named it or a member told this node.
func (n *Node) ProjectMeta() ProjectMeta {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.meta
}

// Rename sets the project's shared name: Lamport one above the current one,
// this node the writer. It is stored, then sent to every member. The first
// rename of a new project (its creator's) also sets CreatedAt.
func (n *Node) Rename(name string) (ProjectMeta, error) {
	if n.cfg.Project == "" {
		return ProjectMeta{}, ErrNotProject
	}
	name, err := NormalizeProjectName(name)
	if err != nil {
		return ProjectMeta{}, err
	}
	n.saveMu.Lock()
	n.mu.Lock()
	next := n.meta
	next.Name, next.Lamport, next.Writer = name, next.Lamport+1, n.id
	if next.CreatedAt.IsZero() {
		next.CreatedAt = time.Now().UTC()
	}
	n.mu.Unlock()
	// saveMu also orders merges: n.meta cannot change until it is released.
	err = writeJSON(filepath.Join(n.cfg.DataDir, projectMetaFile), next)
	if err == nil {
		n.mu.Lock()
		n.meta = next
		n.mu.Unlock()
	}
	n.saveMu.Unlock()
	if err != nil {
		return ProjectMeta{}, err
	}
	n.projectChanged(next)
	return next, nil
}

// mergeProjectMeta applies a meta a member sent, when it wins by LWW.
func (n *Node) mergeProjectMeta(r *ProjectMeta) {
	if n.cfg.Project == "" || r == nil || r.ID != n.cfg.Project || r.Lamport == 0 || !validID(r.Writer) {
		return
	}
	name, err := NormalizeProjectName(r.Name)
	if err != nil || name != r.Name {
		return
	}
	n.saveMu.Lock()
	n.mu.Lock()
	cur := n.meta
	n.mu.Unlock()
	if !metaNewer(*r, cur) {
		n.saveMu.Unlock()
		return
	}
	m := *r
	if m.CreatedAt.IsZero() || (!cur.CreatedAt.IsZero() && cur.CreatedAt.Before(m.CreatedAt)) {
		m.CreatedAt = cur.CreatedAt
	}
	if err := writeJSON(filepath.Join(n.cfg.DataDir, projectMetaFile), m); err != nil {
		n.saveMu.Unlock()
		n.log.Warn("save project meta", "err", err)
		return
	}
	n.mu.Lock()
	n.meta = m
	n.mu.Unlock()
	n.saveMu.Unlock()
	n.projectChanged(m)
}

// projectChanged sends m to every CapProjects session and reports the change.
func (n *Node) projectChanged(m ProjectMeta) {
	n.mu.Lock()
	var to []*peerConn
	for _, pc := range n.conns {
		if pc.has(CapProjects) {
			to = append(to, pc)
		}
	}
	n.mu.Unlock()
	for _, pc := range to {
		n.wg.Go(func() {
			if pc.write(frame{Type: frameProject, ProjectMeta: &m}) != nil {
				pc.close()
			}
		})
	}
	n.changed("project")
}

// sendProject sends the project meta to a new session, once it is known.
func (n *Node) sendProject(pc *peerConn) {
	if n.cfg.Project == "" || !pc.has(CapProjects) {
		return
	}
	m := n.ProjectMeta()
	if m.Lamport == 0 {
		return
	}
	if pc.write(frame{Type: frameProject, ProjectMeta: &m}) != nil {
		pc.close()
	}
}

// HoldWithoutFolder is the inbound hook of a project node without a bound
// folder (NeedsFolder), which runs no worker: every chat request that asks
// this node gets a terminal JobHeld status HoldNoFolder, so its sender sees
// why no agent answers. The request stays unread for a person here. The
// status id is derived from the request, so a resent request is safe.
func (n *Node) HoldWithoutFolder(m Message) error {
	if !m.Asks(n.cfg.Node) || m.From == n.cfg.Node {
		return nil
	}
	if c, ok := n.chats.get(m.ChatID); !ok || c.Closed() {
		return nil // nothing to answer in a chat that is gone or finished
	}
	_, err := n.SendMessage(Message{ID: DerivedID(m.ID, n.cfg.Node+"/held"), ChatID: m.ChatID, ReplyTo: m.ID,
		Kind: KindStatus, JobStatus: JobHeld, HoldReason: HoldNoFolder, Activity: HoldText(HoldNoFolder)})
	return err
}
