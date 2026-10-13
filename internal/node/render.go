package node

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// The text one delivery of unread messages hands an agent session, and the
// line its person sees: rendered here for every client, so the shell hooks
// (agentlink hook), the node's own wakes and Claude Code's in-process module
// (POST /render) read the same and none of them formats a message itself.

// Render modes (RenderRequest.Mode).
const (
	// RenderBatch: the unread messages of the session's folder, inside a turn
	// or with a prompt (the hooks' additionalContext).
	RenderBatch = ""
	// RenderStop: the same, as the reason a Stop hook keeps the turn going.
	RenderStop = "stop"
	// RenderWake: the prompt that wakes an idle session (WakePrompt), carrying
	// Token (WakeMarker).
	RenderWake = "wake"
	// RenderNudge: the prompt that starts a turn for Total messages the
	// conversation holds already (they joined a turn after its last model
	// request, so nothing read them): it names no message again, only Token.
	RenderNudge = "nudge"
)

// RenderRequest is the body of POST /render: unread messages, already granted
// to the session (Claim), to put into words.
type RenderRequest struct {
	Messages []UnreadMessage `json:"messages"`
	// Total is how many unread messages match in all (at least the ones
	// given): the text says how many are left.
	Total int    `json:"total,omitempty"`
	Mode  string `json:"mode,omitempty"`
	// Token is the wake token of a RenderWake prompt.
	Token     string `json:"token,omitempty"`
	Folder    string `json:"folder,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Agent: the subagent whose run reads the text (only its replies are
	// kept); LiveAgents: the session's live subagents, for the main agent's
	// run (their replies are left out, an ended one's is marked as such).
	Agent      string   `json:"agent,omitempty"`
	LiveAgents []string `json:"live_agents,omitempty"`
	// Notes are lines for the model that lead the text (AckResult.Note).
	Notes []string `json:"notes,omitempty"`
	// CLI adds the line naming this node's agentlink executable (a session's
	// first text: agentlink need not be on the agent's own PATH).
	CLI bool `json:"cli,omitempty"`
	// Kept: Messages are the run's own already, an ended subagent's reply
	// marked (the shell hooks pick them themselves, hookSession.forMe): Agent
	// and LiveAgents are not applied a second time. Never read from a request.
	Kept bool `json:"-"`
}

// RenderedMessage names one message the rendered text carries.
type RenderedMessage struct {
	ID      string `json:"id"`
	Project string `json:"project,omitempty"`
}

// RenderResult is the answer of POST /render.
type RenderResult struct {
	// Text is for the model; "" when there is nothing to say.
	Text string `json:"text"`
	// Notice is the line for the person at the session.
	Notice string `json:"notice,omitempty"`
	// Shown are the messages Text carries, in order: the first that fit
	// FormatBudget. The others stay unread for the next delivery.
	Shown []RenderedMessage `json:"shown"`
	// Chats: chat id -> the message of it the session works on now (the
	// newest shown), which its activity is reported for.
	Chats map[string]string `json:"chats,omitempty"`
	// Rest is how many unread messages Text does not carry.
	Rest int `json:"rest,omitempty"`
}

// maxRenderMessages bounds the messages of one RenderRequest.
const maxRenderMessages = 1000

// Render puts req's messages into words (RenderResult).
func Render(req RenderRequest) (RenderResult, error) {
	switch {
	case len(req.Messages) > maxRenderMessages:
		return RenderResult{}, fmt.Errorf("%w: at most %d messages", ErrBadRequest, maxRenderMessages)
	case req.Mode != RenderBatch && req.Mode != RenderStop && req.Mode != RenderWake && req.Mode != RenderNudge:
		return RenderResult{}, fmt.Errorf("%w: invalid mode %q", ErrBadRequest, req.Mode)
	case (req.Mode == RenderWake || req.Mode == RenderNudge) && !validWakeToken(req.Token):
		return RenderResult{}, fmt.Errorf("%w: invalid wake token", ErrBadRequest)
	case len(req.Notes) > 100:
		return RenderResult{}, fmt.Errorf("%w: too many notes", ErrBadRequest)
	}
	if req.Mode == RenderNudge {
		text := fmt.Sprintf("agent-link: выше в этом разговоре уже есть новые сообщения agent-link (%d): они пришли под конец прошлого хода, их ещё никто не читал. Прочитайте их и, где просят ответа, ответьте. %s",
			max(req.Total, len(req.Messages)), WakeMarker(req.Token))
		return RenderResult{Text: WithNotes(req.Notes, text), Shown: []RenderedMessage{}}, nil
	}
	msgs := req.Messages
	if !req.Kept {
		msgs = forAgentRun(msgs, req.Agent, req.LiveAgents)
	}
	total := max(req.Total, len(req.Messages)) - (len(req.Messages) - len(msgs))
	res := RenderResult{Shown: []RenderedMessage{}, Chats: map[string]string{}}
	shown := msgs[:FitUnread(msgs)]
	if len(msgs) == 0 {
		shown = nil
	}
	for _, m := range shown {
		res.Shown = append(res.Shown, RenderedMessage{ID: m.ID, Project: m.Project})
		// Every chat the session reads from shows what it does next (its
		// person's own messages and what it must not answer excepted).
		if m.ChatID != "" && !m.OwnHuman && !m.Paused && m.Assigned != "worker" {
			res.Chats[m.ChatID] = m.ID
		}
	}
	res.Rest = total - len(shown)
	text := ""
	switch {
	case len(shown) == 0:
	case req.Mode == RenderWake:
		text = WakePrompt(shown, res.Rest, req.Folder, req.Token)
	default:
		var t strings.Builder
		if req.Mode == RenderStop {
			t.WriteString("Пока вы работали, пришли сообщения agent-link. Прочитайте их и, где просят ответа, ответьте; затем завершайте.\n")
		} else {
			fmt.Fprintf(&t, "agent-link: непрочитанные сообщения для этой папки (%d). Вся переписка остаётся в истории чата.\n", total)
		}
		for _, m := range shown {
			t.WriteString("\n")
			t.WriteString(FormatUnread(m))
		}
		if res.Rest > 0 {
			fmt.Fprintf(&t, "\nЕщё %d непрочитанных придут со следующим событием. Прочитать сейчас: agentlink chat unread --folder %q --after %s ; прочитанные подтвердить: agentlink chat ack --ids <id,...> --session %s\n",
				res.Rest, req.Folder, shown[len(shown)-1].Cursor, req.SessionID)
		}
		text = strings.TrimRight(t.String(), "\n")
	}
	res.Text = WithNotes(req.Notes, text)
	if req.CLI {
		if exe := cliExecutable(); exe != "" {
			res.Text = strings.TrimSpace(res.Text + "\n\n" + CLIHint(exe))
		}
	}
	res.Notice = BatchNotice(shown)
	return res, nil
}

// forAgentRun keeps the messages a run delivers: a subagent's run (agent)
// only the replies for it; the main agent's none for a live subagent, and
// the replies for an ended one marked as such.
func forAgentRun(msgs []UnreadMessage, agent string, live []string) []UnreadMessage {
	kept := make([]UnreadMessage, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case agent != "":
			if m.ForAgent != agent {
				continue
			}
		case m.ForAgent != "":
			if slices.Contains(live, m.ForAgent) {
				continue
			}
			m.Body = EndedAgentNote(m.ForAgentType, m.ForAgent) + "\n" + m.Body
		}
		kept = append(kept, m)
	}
	return kept
}

// EndedAgentNote heads a reply whose asker, subagent id of type typ, ended:
// the main agent reads it instead.
func EndedAgentNote(typ, id string) string {
	name := id
	if typ != "" {
		name = typ + "/" + id
	}
	return "[ответ для завершившегося субагента " + name + "]"
}

// WithNotes is text led by notes, the lines for the model that wait for its
// next delivery.
func WithNotes(notes []string, text string) string {
	if len(notes) == 0 {
		return text
	}
	return strings.TrimSpace(strings.Join(notes, "\n") + "\n\n" + text)
}

// WorkerNote tells a session that message id of chat, which it was told
// about, is the node's worker's now.
func WorkerNote(id, chat string) string {
	return fmt.Sprintf("agent-link: сообщение %s в чате %s уже взял агент-обработчик этого узла — не отвечайте на него.", id, chat)
}

// CLIHint is the one line naming the agentlink CLI's absolute path exe.
func CLIHint(exe string) string {
	return `agent-link CLI (если agentlink нет в PATH): "` + exe + `", в PowerShell: & "` + exe + `" <команда>`
}

// BatchNotice is the line the person sees: «agent-link: 2 сообщения от
// KPECTIK — беру в работу». The person's own messages need no line.
func BatchNotice(msgs []UnreadMessage) string {
	var from []string
	n, mine, worker := 0, false, true
	for _, m := range msgs {
		if m.OwnHuman {
			continue
		}
		n++
		if !slices.Contains(from, m.From) {
			from = append(from, m.From)
		}
		if m.Assigned != "worker" {
			worker = false
			if m.AsksYou && !m.Paused {
				mine = true
			}
		}
	}
	if n == 0 {
		return ""
	}
	what := "к сведению"
	switch {
	case mine:
		what = "беру в работу"
	case worker:
		what = "отвечает агент-обработчик"
	}
	return fmt.Sprintf("agent-link: %d %s от %s — %s", n, Plural(n, "сообщение", "сообщения", "сообщений"), strings.Join(from, ", "), what)
}

// Plural picks the Russian form for n: 1 сообщение, 2 сообщения, 5 сообщений.
func Plural(n int, one, few, many string) string {
	switch n10, n100 := n%10, n%100; {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	}
	return many
}

// cliExecutable is the agentlink executable this node runs as, which its
// agents run as their CLI; tests replace it.
var cliExecutable = func() string {
	exe, _ := os.Executable()
	return exe
}
