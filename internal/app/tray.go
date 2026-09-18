package app

import "strconv"

// Tray menu labels (uiStrings "tray.*"), for cmd/agentlink-tray.
const (
	TrayStarting     = "tray.starting"
	TrayOpenSettings = "tray.open_settings"
	TrayOpenInbox    = "tray.open_inbox"
	TrayMembers      = "tray.members"
	TrayMemberOn     = "tray.member_on"
	TrayMemberOff    = "tray.member_off"
	TrayQuit         = "tray.quit"
)

// Summary is the one-line state for the tray menu and its tooltip.
func (s Status) Summary() string {
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
