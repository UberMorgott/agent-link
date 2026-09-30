package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentlink.exe")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced := fileReplaced(path)
	if replaced() {
		t.Fatal("unchanged executable appeared replaced")
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if replaced() {
		t.Fatal("missing executable appeared replaced during rename")
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !replaced() {
		t.Fatal("new executable at the same path was missed")
	}
}

// One swap is one "updated" rewake per session: the waiter the next Stop
// starts from the new file sees no replacement, and only a further swap (the
// next release) is seen again.
func TestFileReplacedOncePerSwap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentlink.exe")
	swap := func(data string) {
		t.Helper()
		if err := os.Rename(path, path+".old"+data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWaiter := fileReplaced(path)
	swap("v2")
	if !oldWaiter() {
		t.Fatal("the waiter of the replaced executable missed the swap")
	}
	newWaiter := fileReplaced(path)
	for range 3 {
		if newWaiter() {
			t.Fatal("the waiter started after the swap saw it again")
		}
	}
	swap("v3")
	if !newWaiter() {
		t.Fatal("the next swap was missed")
	}
}

func TestFileReplacedWithoutInitialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.exe")
	replaced := fileReplaced(path)
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if replaced() {
		t.Fatal("missing initial file must not trigger a replacement")
	}
}

func TestFileReplacedBeforeFirstPoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentlink.exe")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced := fileReplaced(path)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !replaced() {
		t.Fatal("replacement before first poll was missed")
	}
}
