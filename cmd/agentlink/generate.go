package main

// icon.ico holds the tray and file icon at 16-256 px; rsrc_windows_amd64.syso
// links it and a manifest into the Windows executable. The "gui" manifest
// only declares per-monitor (v2) DPI awareness and common controls v6, so
// Windows draws the tray, menus and dialogs at the screen's real resolution
// instead of stretching a 96-dpi bitmap; it does not change the PE subsystem,
// which stays console (see AGENTS.md). Regenerate both after changing
// scripts/mkicon:
//
//	go generate ./cmd/agentlink
//
//go:generate go run ../../scripts/mkicon icon.ico
//go:generate go run github.com/tc-hib/go-winres@v0.3.3 simply --manifest gui --icon icon.ico --arch amd64 --out rsrc
