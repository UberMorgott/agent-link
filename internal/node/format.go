package node

import (
	"cmp"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The text an agent session gets for its unread messages: the hooks inject it
// (agentlink hook) and the node queues it as the prompt that wakes an idle
// session (wakeIdle), so both read the same.

// Limits of the text for a session.
const (
	// FormatBudget: runes of message text in one batch (Codex: ~2500 tokens
	// per hook output).
	FormatBudget = 4500
	// FormatMaxBody: runes of one message body.
	FormatMaxBody = 2500
)

// FormatUnread is one message for the model: who wrote it, where, the text
// and what to do with it.
func FormatUnread(m UnreadMessage) string {
	var b strings.Builder
	at := m.CreatedAt.Local().Format("2006-01-02 15:04")
	from := AuthorName(m.Message)
	switch {
	case m.OwnHuman:
		fmt.Fprintf(&b, "Ваш человек написал всем (%s, чат %s, id %s) — к сведению, отвечать не нужно:\n", at, m.ChatID, m.ID)
	case m.ChatID != "":
		fmt.Fprintf(&b, "От %s (%s) в чате %s (участники: %s), id %s, %s:\n", from, authorKind(m.AuthorKind), m.ChatID, strings.Join(m.Participants, ", "), m.ID, at)
	default:
		fmt.Fprintf(&b, "От %s (%s), id %s, %s:\n", from, authorKind(m.AuthorKind), m.ID, at)
	}
	b.WriteString(capBody(m))
	b.WriteString("\n")
	b.WriteString(attachmentLines(m.Attachments))
	if m.OwnHuman {
		return b.String()
	}
	reply := fmt.Sprintf("agentlink send --to %s --reply-to %s --body \"<текст>\"", m.From, m.ID)
	if m.ChatID != "" {
		reply = fmt.Sprintf("agentlink send --chat %s --reply-to %s --body \"<текст>\"", m.ChatID, m.ID)
	}
	switch {
	case m.Assigned == "worker":
		b.WriteString("Это уже обрабатывает агент-обработчик этого узла — не отвечайте.\n")
	case m.Paused:
		b.WriteString("К сведению, ответ не требуется.\n")
	case m.AsksYou:
		fmt.Fprintf(&b, "Просит ответа от вас. Ответить: %s\n", reply)
	default:
		fmt.Fprintf(&b, "К сведению, ответ не обязателен. Ответить: %s\n", reply)
	}
	return b.String()
}

// FitUnread is how many of msgs, from the first, fit FormatBudget runes
// formatted (at least one).
func FitUnread(msgs []UnreadMessage) int {
	n, used := 0, 0
	for _, m := range msgs {
		r := utf8.RuneCountInString(FormatUnread(m))
		if n > 0 && used+r > FormatBudget {
			break
		}
		used += r
		n++
	}
	return n
}

// WakePrompt is the prompt that wakes an idle session for msgs (the ones it
// was granted, at most FitUnread of its unread): the messages themselves, as
// the hooks deliver them; rest more unread stay for its hooks. It carries
// the wake's token (WakeMarker), by which the hooks know the prompt.
func WakePrompt(msgs []UnreadMessage, rest int, folder, token string) string {
	var t strings.Builder
	fmt.Fprintf(&t, "agent-link: новые сообщения (%d). Вся переписка остаётся в истории чата. %s\n", len(msgs), WakeMarker(token))
	for _, m := range msgs {
		t.WriteString("\n")
		t.WriteString(FormatUnread(m))
	}
	if rest > 0 {
		fmt.Fprintf(&t, "\nЕщё %d непрочитанных: agentlink chat unread --folder %q\n", rest, folder)
	}
	return strings.TrimRight(t.String(), "\n")
}

// WakeMarker is the line part by which a wake prompt with token is known.
func WakeMarker(token string) string { return "[agent-link wake " + token + "]" }

// WokenBy reports whether prompt is the wake prompt that carried woken
// message m (UnreadPage.Woken): it has m's wake token and m's id. An empty
// prompt (the agent did not say) or another prompt is not.
func WokenBy(prompt string, m UnreadMessage) bool {
	return m.WakeToken != "" && strings.Contains(prompt, WakeMarker(m.WakeToken)) && strings.Contains(prompt, "id "+m.ID)
}

// AuthorName is who wrote m as people see it: its node, and the local agent
// that wrote it ("Morgott · Codex").
func AuthorName(m Message) string {
	if m.Agent == nil {
		return m.From
	}
	return m.From + " · " + cmp.Or(m.Agent.Label, ProviderName(m.Agent.Provider))
}

func authorKind(k string) string {
	switch k {
	case "human":
		return "человек"
	case "agent":
		return "агент"
	case "worker":
		return "агент-обработчик"
	}
	return "автор не указан"
}

// attachmentLines lists a message's attachments by absolute path, for the
// model to open (images and text/PDF with its file tools).
func attachmentLines(atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Вложения (откройте по пути: изображения и PDF — средством просмотра файлов, текст — чтением файла):\n")
	for _, a := range atts {
		if a.Failed || a.Path == "" {
			fmt.Fprintf(&b, "- %s — не получено\n", a.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s (%s, %s)\n", a.Path, a.MIME, sizeText(a.Size))
	}
	return b.String()
}

// sizeText is a byte count for people: 512 Б, 12 КБ, 3.4 МБ.
func sizeText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d Б", n)
	case n < 1<<20:
		return fmt.Sprintf("%d КБ", n>>10)
	}
	return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
}

// capBody cuts a long body, pointing at the command that shows all of it. The
// attachments' fallback lines are left out: they are listed by path.
func capBody(m UnreadMessage) string {
	body := BodyText(m.Body, m.Attachments)
	if utf8.RuneCountInString(body) <= FormatMaxBody {
		return body
	}
	full := "agentlink inbox"
	if m.ChatID != "" {
		full = "agentlink chat history --chat " + m.ChatID
	}
	r := []rune(body)
	return string(r[:FormatMaxBody]) + fmt.Sprintf("\n[… обрезано, всего %d символов; полностью: %s]", len(r), full)
}
