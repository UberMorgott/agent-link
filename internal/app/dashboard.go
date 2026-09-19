package app

import (
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// ConversationSummary is the latest real message exchanged with one peer.
type ConversationSummary struct {
	Peer      string    `json:"peer"`
	Preview   string    `json:"preview"`
	LatestAt  time.Time `json:"latest_at"`
	Direction string    `json:"direction"`
	Status    string    `json:"status,omitempty"`
}

// ParticipantView combines a known member with its local message history.
type ParticipantView struct {
	node.MemberInfo
	Sent            int       `json:"sent"`
	Received        int       `json:"received"`
	Total           int       `json:"total"`
	LatestAt        time.Time `json:"latest_at,omitzero"`
	LatestPreview   string    `json:"latest_preview,omitempty"`
	LatestDirection string    `json:"latest_direction,omitempty"`
}

// DashboardSummary contains the activity visible on the dashboard overview.
type DashboardSummary struct {
	Status           Status                `json:"status"`
	SentMessages     int                   `json:"sent_messages"`
	ReceivedMessages int                   `json:"received_messages"`
	TotalMessages    int                   `json:"total_messages"`
	ActiveRequests   int                   `json:"active_requests"`
	Recent           []ConversationSummary `json:"recent"`
}

// buildParticipants preserves the member order, excluding this node, and
// augments every known remote member with counts from real message entries.
func buildParticipants(status Status, entries []node.Entry) []ParticipantView {
	participants := make([]ParticipantView, 0, len(status.Members))
	byName := make(map[string]int, len(status.Members))
	for _, member := range status.Members {
		if member.Self {
			continue
		}
		byName[member.Name] = len(participants)
		participants = append(participants, ParticipantView{MemberInfo: member})
	}
	latestIDs := make([]string, len(participants))
	for _, entry := range entries {
		if entry.Kind == node.KindStatus {
			continue
		}
		i, ok := byName[entry.Peer]
		if !ok {
			continue
		}
		participant := &participants[i]
		if entry.Direction == "out" {
			participant.Sent++
		} else {
			participant.Received++
		}
		participant.Total++
		// Message IDs are unique stable hex strings. For equal timestamps, the
		// lexicographically greater ID wins so map-backed history order is irrelevant.
		if entry.CreatedAt.After(participant.LatestAt) ||
			(entry.CreatedAt.Equal(participant.LatestAt) && entry.ID > latestIDs[i]) {
			participant.LatestAt, participant.LatestPreview = entry.CreatedAt, entry.Body
			participant.LatestDirection = entry.Direction
			latestIDs[i] = entry.ID
		}
	}
	return participants
}

// buildDashboard aggregates all locally stored real messages. Recent contains
// at most five peers, ordered by their latest message activity.
func buildDashboard(status Status, entries []node.Entry) DashboardSummary {
	summary := DashboardSummary{Status: status, Recent: []ConversationSummary{}}
	byPeer := make(map[string]ConversationSummary)
	for _, entry := range entries {
		if entry.Kind == node.KindStatus {
			continue
		}
		if entry.Direction == "out" {
			summary.SentMessages++
			if entry.IsRequest() && (entry.JobStatus == node.JobQueued || entry.JobStatus == node.JobRunning) {
				summary.ActiveRequests++
			}
		} else {
			summary.ReceivedMessages++
		}
		summary.TotalMessages++
		if entry.Peer == "" {
			continue
		}
		conversation, ok := byPeer[entry.Peer]
		if !ok || entry.CreatedAt.After(conversation.LatestAt) {
			byPeer[entry.Peer] = ConversationSummary{
				Peer: entry.Peer, Preview: entry.Body, LatestAt: entry.CreatedAt,
				Direction: entry.Direction, Status: entryStatus(entry),
			}
		}
	}
	for _, conversation := range byPeer {
		summary.Recent = append(summary.Recent, conversation)
	}
	slices.SortFunc(summary.Recent, func(a, b ConversationSummary) int {
		if byLatest := b.LatestAt.Compare(a.LatestAt); byLatest != 0 {
			return byLatest
		}
		return strings.Compare(a.Peer, b.Peer)
	})
	if len(summary.Recent) > 5 {
		summary.Recent = summary.Recent[:5]
	}
	return summary
}

func entryStatus(entry node.Entry) string {
	if entry.JobStatus != "" {
		return entry.JobStatus
	}
	return entry.Status
}
