package app

import (
	"strconv"
	"unicode/utf16"

	"github.com/UberMorgott/agent-link/internal/humantime"
)

// Tray menu labels (uiStrings "tray.*"), for the desktop app (cmd/agentlink/app.go).
const (
	TrayAutostart   = "tray.autostart"
	TrayOpenBrowser = "tray.open_browser"
	TrayQuit        = "tray.quit"
	TrayStopAll     = "tray.stop_all"
	TrayResumeAll   = "tray.resume_all"
)

// TrayActionLabel describes the next global agent action, not the current state.
func TrayActionLabel(stopped bool) string {
	if stopped {
		return Text(TrayResumeAll, nil)
	}
	return Text(TrayStopAll, nil)
}

// maxTooltip is the tray tooltip's room in UTF-16 units, without the
// terminating zero (NOTIFYICONDATAW.szTip holds 128).
const maxTooltip = 127

// TrayTooltip is the tray icon's tooltip: the state, and a newer version when
// one can be installed or when GitHub rate-limits the update, e.g. "agentlink — На связи 7 из 12 · доступна v0.6.0".
func TrayTooltip(s Status, u UpdateStatus) string {
	t := "agentlink — " + s.Summary()
	switch {
	case u.Failed && !u.RetryAt.IsZero():
		t += " · " + msg("tray.update_retry", map[string]string{"time": humantime.Format(u.RetryAt)})
	case u.Available && u.Latest != "":
		t += " · " + msg("tray.update", map[string]string{"version": u.Latest})
	}
	if len(utf16.Encode([]rune(t))) <= maxTooltip {
		return t
	}
	r := []rune(t)
	for len(utf16.Encode(r)) > maxTooltip-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// Summary is the one-line state for the tray tooltip, with the weak-code
// warning after it.
func (s Status) Summary() string {
	if s.Warning == "link.weak_code" {
		return s.state() + " · " + msg("tray.weak_code", nil)
	}
	return s.state()
}

func (s Status) state() string {
	vars := map[string]string{"peer": s.Peer, "online": strconv.Itoa(s.Online), "total": strconv.Itoa(s.Total)}
	switch {
	case !s.Configured:
		return msg("tray.not_set_up", nil)
	case s.Error != "":
		return msg("tray.error", nil)
	case s.Connected && s.Total > 1:
		return msg("tray.connected_n", vars)
	case s.Connected:
		return msg("tray.connected", vars)
	case s.Problem != "" || s.Peer == "":
		return msg("tray.not_connected", nil)
	default:
		return msg("tray.lost", vars)
	}
}
