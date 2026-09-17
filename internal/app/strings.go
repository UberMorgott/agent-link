package app

import (
	"encoding/json"
	"html"
)

// uiStrings is the single place holding every user-visible string of the web
// UI. The pages carry no text of their own: HTML marks each spot with
// data-t="<key>" and the scripts look keys up through t()/fmt(). Swapping this
// map (or picking one of several) is all a future language switch needs.
//
// Keys are English identifiers; only the values are shown to the user.
// {name} placeholders are filled in by fmt() in the page scripts.
var uiStrings = map[string]string{
	// Navigation, page titles and the connection indicator.
	"nav.settings":         "Настройки",
	"nav.inbox":            "Сообщения",
	"page.title.settings":  "agentlink — настройки",
	"page.title.inbox":     "agentlink — сообщения",
	"link.on":              "связь есть: {peer}",
	"link.off":             "нет связи: {peer}",
	"link.unconfigured":    "не настроено",
	"link.error":           "ошибка: {error}",
	"link.app_not_running": "agentlink не запущен",

	// Settings page.
	"settings.h1":                 "Настройки",
	"settings.self.legend":        "Этот компьютер",
	"settings.node.label":         "Моё имя",
	"settings.node.hint":          "Короткое имя этого компьютера латиницей, например morgott; собеседник указывает его у себя.",
	"settings.listen.label":       "Мой адрес",
	"settings.listen.hint":        "Свой адрес в ZeroTier и порт, например 10.147.20.5:7420.",
	"settings.peer.legend":        "Собеседник",
	"settings.peer_name.label":    "Его имя",
	"settings.peer_name.hint":     "То имя, которое он написал у себя в поле «Моё имя».",
	"settings.peer_addr.label":    "Его адрес",
	"settings.peer_addr.hint":     "Его адрес в ZeroTier и порт, например 10.147.20.9:7420.",
	"settings.secret.label":       "Общий пароль",
	"settings.secret.hint":        "Тот же секрет, что у собеседника: один нажимает «Создать» и передаёт его лично, другой вставляет.",
	"settings.secret.show":        "Показать",
	"settings.secret.generate":    "Создать",
	"settings.secret.copy":        "Скопировать",
	"settings.areas.label":        "Общие темы",
	"settings.areas.hint":         "Необязательно, через запятую: dev, design. Тогда сообщение можно послать на area:dev.",
	"settings.handler.legend":     "Ответы на вопросы",
	"settings.handler.label":      "Кто отвечает",
	"settings.handler.hint":       "Входящий вопрос запускает выбранного агента в рабочей папке, только на чтение, и его ответ уходит сам.",
	"settings.handler.none":       "Никто, отвечаю сам",
	"settings.handler.claude":     "Claude Code",
	"settings.handler.codex":      "Codex",
	"settings.work_dir.label":     "Рабочая папка",
	"settings.work_dir.hint":      "Папка проекта, в которой агент ищет ответ, например E:\\DEV\\CodeDungeon.",
	"settings.autostart.label":    "Запускать agentlink при входе в Windows",
	"settings.save":               "Сохранить",
	"settings.saving":             "Сохраняю…",
	"settings.saved":              "Сохранено, agentlink перезапущен с новыми настройками.",
	"settings.save_failed":        "Не сохранено: {error}",
	"settings.load_failed":        "Не удалось загрузить настройки: {error}",
	"settings.secret.generated":   "Новый пароль создан. Скопируйте его, передайте собеседнику лично и нажмите «Сохранить».",
	"settings.secret.copied":      "Пароль скопирован.",
	"settings.secret.copy_manual": "Нажмите Ctrl+C, чтобы скопировать выделенный пароль.",

	// Inbox page: the send form.
	"inbox.h1":           "Сообщения",
	"inbox.send.legend":  "Новый вопрос",
	"inbox.to.label":     "Кому",
	"inbox.to.hint":      "Имя собеседника или area:тема для рассылки по теме.",
	"inbox.body.label":   "Текст",
	"inbox.body.hint":    "Вопрос, который получит агент на другом компьютере.",
	"inbox.send":         "Отправить",
	"inbox.replying":     "Ответ на сообщение {id}",
	"inbox.cancel_reply": "Отменить ответ",
	"inbox.sent":         "Отправлено, стоит в очереди.",
	"inbox.send_failed":  "Не отправлено: {error}",
	"inbox.load_failed":  "Не удалось загрузить сообщения: {error}",
	"inbox.empty":        "Пока ни одного сообщения.",

	// Inbox page: one request with its answer.
	"inbox.dir.out":   "Исходящее",
	"inbox.dir.in":    "Входящее",
	"inbox.route":     "{from} → {to}",
	"inbox.area":      "тема: {area}",
	"inbox.question":  "Вопрос",
	"inbox.answer":    "Ответ",
	"inbox.no_answer": "Ответа пока нет.",
	"inbox.reply":     "Ответить",
	"inbox.status":    "Статус: {status}",

	// Statuses, keyed by the wire value in Thread.Status.
	"status.queued":    "в очереди",
	"status.running":   "выполняется",
	"status.completed": "готово",
	"status.failed":    "ошибка",
	"status.answered":  "есть ответ",
	"status.pending":   "ждёт обработки",
	"status.delivered": "передано агенту",
	"status.sent":      "доставлено",
}

// stringsAttr renders uiStrings as JSON escaped for an HTML attribute value.
func stringsAttr() string {
	data, err := json.Marshal(uiStrings)
	if err != nil { // a map[string]string always marshals
		return "{}"
	}
	return html.EscapeString(string(data))
}
