//go:build !windows

package node

import (
	"io/fs"
	"path/filepath"
)

// isReparsePoint: only Windows has reparse points; a link shows in the mode.
func isReparsePoint(st fs.FileInfo) bool { return st.Mode()&fs.ModeSymlink != 0 }

// realDir is folder p with every link resolved.
func realDir(p string) (string, error) { return filepath.EvalSymlinks(p) }
