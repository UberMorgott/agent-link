//go:build !windows

package node

import (
	"io/fs"
	"os"
	"path/filepath"
)

// readShared reads file p; elsewhere an open file never blocks its rename.
func readShared(p string) ([]byte, error) { return os.ReadFile(p) } //nolint:gosec // G304: the node's own data folder

// isReparsePoint: only Windows has reparse points; a link shows in the mode.
func isReparsePoint(st fs.FileInfo) bool { return st.Mode()&fs.ModeSymlink != 0 }

// realDir is folder p with every link resolved.
func realDir(p string) (string, error) { return filepath.EvalSymlinks(p) }
