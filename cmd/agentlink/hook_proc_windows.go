//go:build windows

package main

import (
	"errors"
	"math"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// agentCommandLine is the command line of the agent process (claude, codex)
// this hook runs under: the nearest ancestor named like client, or a node.exe
// running it. nil when there is none or it cannot be read.
func agentCommandLine(client string) []string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	type proc struct {
		parent uint32
		name   string
	}
	procs := map[uint32]proc{}
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		name := strings.TrimSuffix(strings.ToLower(windows.UTF16ToString(e.ExeFile[:])), ".exe")
		procs[e.ProcessID] = proc{parent: e.ParentProcessID, name: name}
	}
	pid := uint32(os.Getpid()) // #nosec G115 -- a Windows pid is a DWORD
	for range 10 {
		p, ok := procs[pid]
		if !ok || p.parent == 0 || p.parent == pid {
			return nil
		}
		pid = p.parent
		name := procs[pid].name
		if name != client && name != "node" {
			continue
		}
		line := processCommandLine(pid)
		if name == "node" && !strings.Contains(strings.ToLower(line), client) {
			continue
		}
		args, err := windows.DecomposeCommandLine(line)
		if err != nil || len(args) == 0 {
			return nil
		}
		return args
	}
	return nil
}

// processCommandLine reads process pid's command line ("" when it cannot).
func processCommandLine(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	const size = 64 << 10
	buf := make([]byte, size)
	var n uint32
	if windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), size, &n) != nil { // #nosec G103 -- NtQueryInformationProcess fills buf
		return ""
	}
	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])) // #nosec G103 -- buf starts with the UNICODE_STRING it wrote
	return us.String()
}

// processAlive reports whether process pid still runs.
func processAlive(pid int) bool {
	if pid <= 0 || pid > math.MaxUint32 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED) // exists, not ours to query
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	const stillActive = 259
	return code == stillActive
}
