// Package gitwt finds the git work tree of a folder and, for a linked
// worktree (git worktree add), its repository's main checkout.
package gitwt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Timeout bounds each git call.
const Timeout = 5 * time.Second

// Tree is the git work tree holding a folder.
type Tree struct {
	// Top is its top folder (where its .git is); "" outside git.
	Top string
	// Main is the main checkout of the repository when Top is a linked
	// worktree, else Top.
	Main string
}

// Linked reports whether the tree is a linked worktree of another checkout.
func (t Tree) Linked() bool { return t.Main != t.Top }

// Of is the work tree holding dir. A .git folder is a checkout of its own; a
// .git file is asked of git, which tells a linked worktree (its git dir is
// not the repository's common dir) from a submodule or a separate git dir.
// When git is missing or fails, Top counts as its own checkout.
func Of(ctx context.Context, dir string) Tree {
	for d := filepath.Clean(dir); ; {
		if st, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			if st.IsDir() {
				return Tree{Top: d, Main: d}
			}
			return Tree{Top: d, Main: mainCheckout(ctx, d)}
		}
		up := filepath.Dir(d)
		if up == d {
			return Tree{}
		}
		d = up
	}
}

// mainCheckout is the main checkout of the linked worktree top, else top.
func mainCheckout(ctx context.Context, top string) string {
	dirs := lines(git(ctx, top, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"))
	if len(dirs) != 2 || filepath.Clean(dirs[0]) == filepath.Clean(dirs[1]) {
		return top
	}
	// The first entry is the main worktree; a bare repository has none.
	list := lines(git(ctx, top, "worktree", "list", "--porcelain"))
	if len(list) < 2 || !strings.HasPrefix(list[0], "worktree ") || list[1] == "bare" {
		return top
	}
	main := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(list[0], "worktree ")))
	if st, err := os.Stat(main); err != nil || !st.IsDir() || !filepath.IsAbs(main) {
		return top
	}
	return main
}

// git runs git in dir, ignoring the caller's GIT_* location variables; ""
// when it fails.
func git(ctx context.Context, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed git subcommands; dir is a folder
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// lines are the non-empty lines of s.
func lines(s string) []string {
	var out []string
	for l := range strings.Lines(s) {
		if l = strings.TrimRight(l, "\r\n"); l != "" {
			out = append(out, l)
		}
	}
	return out
}
