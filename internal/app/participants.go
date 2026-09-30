package app

import (
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

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
