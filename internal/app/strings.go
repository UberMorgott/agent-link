package app

import (
	"encoding/json"
	"html"
	"strings"
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
	"link.waiting":         "ждём собеседника",
	"link.unconfigured":    "не настроено",
	"link.app_not_running": "agentlink не запущен",
	"link.no_code":         "нет кода связи — нажмите «Создать код» или впишите код собеседника",
	"link.no_peer":         "нет собеседника — впишите его адрес",
	"link.bad_code":        "код связи не совпадает с кодом собеседника — сверьте его у обоих",
	"link.same_name":       "у собеседника то же имя, что у вас, или это ваш собственный адрес — поменяйте имя или адрес",
	"link.wrong_peer":      "по этому адресу отвечает не тот собеседник — проверьте адрес или «Имя собеседника» в «Дополнительно»",
	"link.no_zerotier":     "ZeroTier не найден",

	// Settings page.
	"settings.h1":                  "Настройки",
	"settings.node.label":          "Ваше имя",
	"settings.node.hint":           "Как вас увидит собеседник, например morgott.",
	"settings.code.label":          "Код связи",
	"settings.code.hint":           "6 латинских букв или цифр, одинаковые у обоих: один нажимает «Создать код» и сообщает его другому.",
	"settings.code.generate":       "Создать код",
	"settings.code.copy":           "Скопировать",
	"settings.code.generated":      "Код создан — сообщите его собеседнику и нажмите «Сохранить».",
	"settings.code.copied":         "Код скопирован.",
	"settings.code.copy_manual":    "Нажмите Ctrl+C, чтобы скопировать выделенный код.",
	"settings.peer_addr.label":     "Адрес собеседника",
	"settings.peer_addr.hint":      "Его IP в ZeroTier, например 10.147.20.9; порт можно не писать.",
	"settings.my_addr":             "Ваш адрес для собеседника: {addr}",
	"settings.my_addr.none":        "ZeroTier не найден — установите ZeroTier, подключитесь к общей сети и перезапустите agentlink.",
	"settings.handler.label":       "Кто отвечает",
	"settings.handler.hint":        "Входящий вопрос запускает выбранного агента в рабочей папке, только на чтение, и его ответ уходит сам.",
	"settings.handler.none":        "Никто, отвечаю сам",
	"settings.handler.claude":      "Claude Code",
	"settings.handler.codex":       "Codex",
	"settings.work_dir.label":      "Рабочая папка",
	"settings.work_dir.hint":       "Папка проекта, в которой агент ищет ответ: нажмите «Выбрать…» и укажите её в окне Windows.",
	"settings.work_dir.pick":       "Выбрать…",
	"settings.work_dir.picking":    "Открыто окно выбора папки Windows — выберите папку в нём.",
	"settings.work_dir.pick_title": "Выберите рабочую папку",
	"settings.work_dir.chosen":     "Выбрана папка: {path} — нажмите «Сохранить».",
	"settings.work_dir.current":    "Сейчас: {path}",
	"settings.work_dir.empty":      "Папка не выбрана.",
	"settings.work_dir.cancelled":  "Отменено — папка не изменилась.",
	"settings.advanced":            "Дополнительно",
	"settings.listen.label":        "Мой адрес",
	"settings.listen.hint":         "Пусто — адрес ZeroTier этого компьютера с портом 7420.",
	"settings.api.label":           "Адрес этой страницы",
	"settings.api.hint":            "Пусто — 127.0.0.1:7520; меняется после перезапуска agentlink.",
	"settings.areas.label":         "Общие темы",
	"settings.areas.hint":          "Через запятую: dev, design. Тогда сообщение можно послать на area:dev.",
	"settings.peer_name.label":     "Имя собеседника",
	"settings.peer_name.hint":      "Пусто — имя берётся из соединения; если заполнено, чужое имя не примется.",
	"settings.autostart.label":     "Запускать agentlink при входе в Windows",
	"settings.save":                "Сохранить",
	"settings.saving":              "Сохраняю…",
	"settings.saved":               "Сохранено.",

	// Errors: each one sentence saying what to do.
	"error.node":             "Впишите ваше имя: буквы, цифры, «-» или «_», без пробелов.",
	"error.code":             "Код связи — ровно 6 латинских букв или цифр: нажмите «Создать код» или впишите код собеседника.",
	"error.peer_addr":        "Адрес собеседника не похож на IP — впишите его адрес в ZeroTier, например 10.147.20.9.",
	"error.work_dir":         "Укажите существующую рабочую папку, в которой агент будет искать ответы, или выберите «Никто, отвечаю сам».",
	"error.handler":          "Выберите, кто отвечает: Claude Code, Codex или никто.",
	"error.handler_missing":  "Выбранный агент не найден на этом компьютере — установите его или, если только что установили, перезапустите agentlink.",
	"error.listen_format":    "«Мой адрес» в «Дополнительно» должен быть IP этого компьютера, например 10.147.20.5, или пустым.",
	"error.listen":           "Не удалось занять адрес {addr} — проверьте «Мой адрес» в «Дополнительно» или закройте другую копию agentlink.",
	"error.api":              "«Адрес этой страницы» в «Дополнительно» должен быть вида 127.0.0.1:7520 или пустым.",
	"error.areas":            "Общие темы в «Дополнительно» — слова из букв и цифр через запятую, например dev, design.",
	"error.peer_name":        "«Имя собеседника» в «Дополнительно» — буквы, цифры, «-» или «_»; можно оставить пустым.",
	"error.peer_name_self":   "«Имя собеседника» совпадает с вашим именем — оставьте его пустым или впишите имя собеседника.",
	"error.start":            "Настройки сохранены, но agentlink не запустился — подробности в файле agentlink.log рядом с настройками.",
	"error.save":             "Не удалось записать настройки на диск — проверьте, что папка %APPDATA%\\agentlink доступна, и попробуйте снова.",
	"error.bad_request":      "Страница прислала неверные данные — обновите её (F5) и попробуйте снова.",
	"error.forbidden":        "Страница устарела — обновите её (F5).",
	"error.no_app":           "agentlink не отвечает — запустите agentlink-tray.exe и обновите страницу.",
	"error.internal":         "Что-то пошло не так — подробности в файле agentlink.log рядом с настройками.",
	"error.not_running":      "agentlink ещё не подключён — впишите код связи в настройках и сохраните.",
	"error.empty_body":       "Напишите текст сообщения.",
	"error.unknown_peer":     "Собеседник ещё ни разу не подключался — дождитесь надписи «связь есть» и отправьте снова.",
	"error.no_area_peer":     "На эту тему никто не подписан — проверьте «Общие темы» у собеседника.",
	"error.send":             "Не удалось отправить сообщение — подробности в файле agentlink.log рядом с настройками.",
	"error.pick_busy":        "Окно выбора папки уже открыто — выберите папку в нём или закройте его.",
	"error.pick_unsupported": "Окно выбора папки есть только в Windows — впишите путь к папке вручную.",
	"error.pick":             "Не удалось открыть окно выбора папки — впишите путь вручную; подробности в файле agentlink.log.",
	// Inbox page: the send form.
	"inbox.h1":           "Сообщения",
	"inbox.send.legend":  "Новый вопрос",
	"inbox.to.label":     "Кому",
	"inbox.to.hint":      "Можно оставить пустым — уйдёт собеседнику; area:тема — рассылка по теме.",
	"inbox.body.label":   "Текст",
	"inbox.body.hint":    "Вопрос, который получит агент на другом компьютере.",
	"inbox.send":         "Отправить",
	"inbox.replying":     "Ответ на сообщение {id}",
	"inbox.cancel_reply": "Отменить ответ",
	"inbox.sent":         "Отправлено, стоит в очереди.",
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

// msg returns the text for key with {name} placeholders filled from vars, the
// server-side twin of fmt() in the page scripts.
func msg(key string, vars map[string]string) string {
	s, ok := uiStrings[key]
	if !ok {
		return uiStrings["error.internal"]
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
