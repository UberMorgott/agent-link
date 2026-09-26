package node

import (
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// readShared reads file p without holding up its rename or removal meanwhile
// (FILE_SHARE_DELETE, which os.ReadFile does not give): the attachment sweep
// reads files the store is replacing by rename.
func readShared(p string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(h), p)
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// isReparsePoint reports whether an Lstat result is a reparse point: a
// symbolic link, a junction or any other redirection of the path.
func isReparsePoint(st fs.FileInfo) bool {
	d, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// realDir is the final path of folder p, every link and junction resolved,
// as Windows itself reports it (filepath.EvalSymlinks does not follow
// junctions).
func realDir(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &fs.PathError{Op: "open", Path: p, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	size := uint32(windows.MAX_PATH)
	buf := make([]uint16, size)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], size, 0)
		if err != nil {
			return "", &fs.PathError{Op: "realpath", Path: p, Err: err}
		}
		if n < size {
			buf = buf[:n]
			break
		}
		size = n + 1
		buf = make([]uint16, size)
	}
	s := windows.UTF16ToString(buf)
	if rest, ok := strings.CutPrefix(s, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(s, `\\?\`), nil
}
