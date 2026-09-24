package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// A process started by a packaged (MSIX) desktop app, such as a terminal or an
// agent session of Claude's desktop app, can run inside that app's AppData
// virtualization: every file it creates under %APPDATA% lands in the app's
// private copy (%LOCALAPPDATA%\Packages\<package>\LocalCache\Roaming) and only
// processes of the same app see it, while existing files keep their real
// version for everyone else. The desktop app writes its settings atomically
// (a new file renamed over the old one), so an instance run there saves into
// the private copy, and the next instance started elsewhere reads the real,
// older file, or the other way round: projects joined in one view are missing
// in the other. The desktop app therefore always runs outside: started inside,
// it starts itself again outside through WMI, whose processes belong to no
// package, and exits.

// sandboxedPrefix marks the probe file that detects the virtualization.
const sandboxedPrefix = ".sandbox-probe-"

// leaveSandbox relaunches the desktop app outside a packaged app's AppData
// virtualization of cfgDir and reports true when this process should exit.
// outside is set on the relaunched instance: still inside then is an error,
// as is running inside with noTray (a script that expects the app to stay in
// its terminal).
func leaveSandbox(cfgDir string, args []string, outside, noTray bool) (bool, error) {
	inside, err := sandboxed(cfgDir, os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"))
	if err != nil || !inside {
		return false, err
	}
	switch {
	case outside:
		return false, fmt.Errorf("%s is still virtualized by a packaged app after starting outside it; start agentlink from the Start menu or Explorer", cfgDir)
	case noTray:
		return false, fmt.Errorf("%s is virtualized by the packaged app this runs in (its files would go to a private copy); run agentlink from a terminal outside that app, or with -config elsewhere", cfgDir)
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if err := startOutside(exe, append(withoutFlag(args, sandboxFlag), "-"+sandboxFlag)); err != nil {
		return false, fmt.Errorf("start agentlink outside the packaged app's AppData virtualization: %w", err)
	}
	return true, nil
}

// withoutFlag returns args without the flag name (any number of dashes).
func withoutFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.TrimLeft(a, "-") != name {
			out = append(out, a)
		}
	}
	return out
}

// sandboxed reports whether a file created in cfgDir, a folder under appData,
// lands in a packaged app's private copy of it under localAppData.
func sandboxed(cfgDir, appData, localAppData string) (bool, error) {
	if appData == "" || localAppData == "" {
		return false, nil
	}
	rel, err := filepath.Rel(appData, cfgDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, nil // a folder outside AppData is never virtualized
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(cfgDir, sandboxedPrefix+strconv.Itoa(os.Getpid())+"-*")
	if err != nil {
		return false, err
	}
	name := filepath.Base(f.Name())
	_ = f.Close()
	defer func() { _ = os.Remove(f.Name()) }()
	return overlayHolds(localAppData, rel, name), nil
}

// overlayHolds reports whether some package's private copy of %APPDATA%
// holds rel\name.
func overlayHolds(localAppData, rel, name string) bool {
	root := filepath.Join(localAppData, "Packages")
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "LocalCache", "Roaming", rel, name)); err == nil {
			return true
		}
	}
	return false
}

// outsideCommandLine is the command line WMI starts: exe and args quoted the
// way CommandLineToArgvW reads them.
func outsideCommandLine(exe string, args []string) string {
	parts := []string{syscall.EscapeArg(exe)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

// outsideScript starts $env:AGENTLINK_OUTSIDE_CMD through WMI in
// $env:AGENTLINK_OUTSIDE_DIR and exits with Win32_Process.Create's result.
const outsideScript = `$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$env:AGENTLINK_OUTSIDE_CMD; CurrentDirectory=$env:AGENTLINK_OUTSIDE_DIR}; exit [int]$r.ReturnValue`

// startOutside starts exe with args as a process of no package: WMI's
// Win32_Process.Create, asked through Windows PowerShell.
func startOutside(exe string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command", outsideScript) // #nosec G204 -- fixed script; the command line travels in the environment
	cmd.Env = append(os.Environ(), "AGENTLINK_OUTSIDE_CMD="+outsideCommandLine(exe, args), "AGENTLINK_OUTSIDE_DIR="+filepath.Dir(exe))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Errorf("process creation through WMI failed with %d: %s", exit.ExitCode(), strings.TrimSpace(string(out)))
	}
	return err
}
