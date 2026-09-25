package main

import (
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// An unread message lists its attachments by absolute path instead of the
// body's fallback lines; one that did not arrive says so.
func TestFormatUnreadAttachments(t *testing.T) {
	m := chatMsg("c1", "KPECTIK", "human", "look\n[attachment: a.png]\n[attachment: gone.txt]", true)
	m.Attachments = []node.Attachment{
		{ID: strings.Repeat("a", 64), Name: "a.png", MIME: node.MIMEPNG, Size: 2048, Path: `C:\proj\.agentlink\attachments\aaaaaaaa-a.png`},
		{ID: strings.Repeat("b", 64), Name: "gone.txt", Failed: true},
	}
	out := node.FormatUnread(m)
	for _, want := range []string{"look\n", `- C:\proj\.agentlink\attachments\aaaaaaaa-a.png (image/png, 2 КБ)`, "- gone.txt — не получено"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[attachment:") {
		t.Errorf("fallback lines kept:\n%s", out)
	}
}
