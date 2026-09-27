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
