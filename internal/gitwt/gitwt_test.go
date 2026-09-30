package gitwt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repo makes a git repository with one commit in a new folder and a linked
// worktree of it outside that folder; it skips the test without git.
func repo(t *testing.T) (main, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	main, worktree = filepath.Join(root, "main"), filepath.Join(root, "wt")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...) // #nosec G204 -- fixed git commands in a test folder
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(main, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	run(main, "init", "-q")
	run(main, "commit", "-q", "--allow-empty", "-m", "init")
	run(main, "worktree", "add", "-q", worktree)
	return main, worktree
}

// same reports whether a and b are one folder (a temp path may be spelled
// short or long on Windows).
func same(t *testing.T, a, b string) bool {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

func TestOf(t *testing.T) {
	main, wt := repo(t)
	sub := filepath.Join(wt, "deep")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if tr := Of(ctx, filepath.Join(main, "sub")); tr.Top != main || tr.Linked() {
		t.Fatalf("main checkout: %+v", tr)
	}
	if tr := Of(ctx, sub); tr.Top != wt || !tr.Linked() || !same(t, tr.Main, main) {
		t.Fatalf("linked worktree: %+v, want main %s", tr, main)
	}
	if tr := Of(ctx, t.TempDir()); tr.Top != "" || tr.Main != "" {
		t.Fatalf("outside git: %+v", tr)
	}
}

// A .git file that is not a linked worktree (here a separate git dir, as a
// submodule has) is a checkout of its own.
func TestOfSeparateGitDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work, gitDir := filepath.Join(root, "work"), filepath.Join(root, "store.git")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "-C", work, "init", "-q", "--separate-git-dir", gitDir).CombinedOutput(); err != nil { // #nosec G204 -- fixed git command in a test folder
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if tr := Of(context.Background(), work); tr.Top != work || tr.Linked() {
		t.Fatalf("separate git dir: %+v", tr)
	}
}

// A .git file git cannot read leaves the folder its own checkout.
func TestOfBrokenGitFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(dir, "missing")), 0o600); err != nil {
		t.Fatal(err)
	}
	if tr := Of(context.Background(), dir); tr.Top != dir || tr.Linked() {
		t.Fatalf("broken .git file: %+v", tr)
	}
}
