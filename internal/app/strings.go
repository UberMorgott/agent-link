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
	"link.on_many":         "на связи {online} из {total}",
	"link.off":             "нет связи: {peer}",
	"link.waiting":         "ждём участников",
	"link.unconfigured":    "не настроено",
	"link.app_not_running": "agentlink не запущен",
	"link.no_code":         "нет кода связи — нажмите «Создать код» или впишите код, который вам дали",
	"link.no_peer":         "пока никого — добавьте адрес участника или дождитесь, пока вас найдут в локальной сети",
	"link.bad_code":        "код связи не совпадает с кодом участника по этому адресу — сверьте его",
	"link.same_name":       "у участника то же имя, что у вас, или это ваш собственный адрес — поменяйте имя или адрес",
	"link.wrong_peer":      "по добавленному адресу отвечает не тот участник — проверьте адрес",
	"link.removed":         "вас удалили из сети — попросите участника снова добавить ваш адрес",
	"link.name_taken":      "ваше имя уже занято другим участником — впишите другое имя и сохраните",
	"link.no_zerotier":     "ZeroTier не найден",
	"link.weak_code":       "старый короткий код связи, а agentlink доступен не только из локальной сети — нажмите «Создать код», сохраните и сообщите новый код участникам",
	"link.legacy_public":   "участник по добавленному адресу — старая версия agentlink, а она подключается только из локальной сети или ZeroTier — попросите его обновиться",

	// Tray menu (cmd/agentlink app.go, see tray.go).
	"tray.starting":      "Запуск…",
	"tray.open_settings": "Открыть настройки",
	"tray.open_inbox":    "Открыть сообщения",
	"tray.members":       "Участники",
	"tray.member_on":     "{name} — на связи",
	"tray.member_off":    "{name} — нет связи",
	"tray.quit":          "Выйти",
	"tray.not_set_up":    "Не настроено — откройте настройки",
	"tray.error":         "Ошибка — откройте настройки",
	"tray.connected":     "{peer} — на связи",
	"tray.connected_n":   "На связи {online} из {total}",
	"tray.not_connected": "Нет связи — откройте настройки",
	"tray.lost":          "{peer} — нет связи",
	"tray.weak_code":     "слабый код — создайте новый",

	// Settings page.
	"settings.h1":                  "Настройки",
	"settings.node.label":          "Ваше имя",
	"settings.node.hint":           "Как вас увидят другие участники, например morgott.",
	"settings.code.label":          "Код связи",
	"settings.code.hint":           "Вида XXXX-XXXX-XXXX, одинаковый у всех участников: один нажимает «Создать код» и сообщает его остальным. Старый код из 6 символов тоже работает, но его можно подобрать — замените его.",
	"settings.code.generate":       "Создать код",
	"settings.code.copy":           "Скопировать",
	"settings.code.generated":      "Код создан — сообщите его участникам и нажмите «Сохранить».",
	"settings.code.copied":         "Код скопирован.",
	"settings.code.copy_manual":    "Нажмите Ctrl+C, чтобы скопировать выделенный код.",
	"settings.members.label":       "Участники сети",
	"settings.members.none":        "Пока никого — добавьте адрес участника ниже или дождитесь, пока участники с тем же кодом найдутся в локальной сети.",
	"settings.members.self":        "{name} (вы)",
	"settings.members.online":      "на связи",
	"settings.members.lost":        "нет связи",
	"settings.members.seen":        "был на связи {when}",
	"settings.members.version":     "версия {version}",
	"settings.members.legacy":      "старая версия — не передаёт список участников",
	"settings.members.old_auth":    "старая версия — вход без защиты кода, только из локальной сети",
	"settings.members.remove":      "Удалить",
	"settings.members.confirm":     "Удалить {name} из сети у всех участников? Вернуть можно, снова добавив его адрес.",
	"settings.members.removed":     "{name} удалён из сети.",
	"settings.peer_addr.label":     "Добавить участника по адресу",
	"settings.peer_addr.hint":      "IP другого участника — в ZeroTier, локальной сети или внешний, например 10.147.20.9; порт можно не писать. Подключится, только если у него тот же код, и его адрес узнают все участники.",
	"settings.peer_addr.add":       "Добавить",
	"settings.peer_addr.added":     "Адрес {addr} добавлен — подключаюсь.",
	"settings.peer_addr.empty":     "Впишите IP участника.",
	"settings.my_addr":             "Ваш адрес для других участников: {addr}",
	"settings.my_addr.none":        "ZeroTier не найден — адрес выше из локальной сети. Для связи через интернет установите ZeroTier или дайте внешний адрес.",
	"settings.handler.label":       "Кто отвечает",
	"settings.handler.hint":        "Входящий вопрос запускает выбранного агента в рабочей папке, только на чтение, и его ответ уходит сам.",
	"settings.handler.none":        "Никто, отвечаю сам",
	"settings.handler.claude":      "Claude Code",
	"settings.handler.codex":       "Codex",
	"settings.agent.label":         "Программа агента",
	"settings.agent.pick":          "Указать…",
	"settings.agent.hint":          "Находится сама, как бы агент ни был установлен: приложение Codex или Claude, npm, установщик, winget, расширение VS Code. Если нет — нажмите «Указать…» и выберите codex.exe / codex.cmd или claude.exe.",
	"settings.agent.find":          "Найти заново",
	"settings.agent.finding":       "Ищу программу агента…",
	"settings.agent.save_hint":     "Нажмите «Сохранить», чтобы запомнить найденную программу.",
	"settings.agent.not_found":     "Не найдена ни в одном известном месте — установите агента или нажмите «Указать…».",
	"settings.agent.from_path":     "Найдена в PATH: {path}",
	"settings.agent.from_setting":  "Указана: {path}",
	"settings.agent.gone":          "Не найдена: {path} — нажмите «Найти заново» или «Указать…».",
	"settings.agent.missing":       "Не найдена — нажмите «Найти заново» или «Указать…».",
	"settings.agent.found":         "Найден: {kind} — {path}",
	"agent_kind.path":              "PATH (путь поиска программ Windows)",
	"agent_kind.env":               "переменная CODEX_CLI_PATH",
	"agent_kind.standalone":        "установщик агента",
	"agent_kind.npm":               "пакет npm",
	"agent_kind.winget":            "пакет winget",
	"agent_kind.codex_app":         "приложение Codex",
	"agent_kind.claude_app":        "приложение Claude",
	"agent_kind.vscode":            "расширение VS Code",
	"agent_kind.vscode_insiders":   "расширение VS Code Insiders",
	"agent_kind.cursor":            "расширение Cursor",
	"agent_kind.windsurf":          "расширение Windsurf",
	"settings.agent.chosen":        "Выбрана программа: {path} — нажмите «Сохранить».",
	"settings.agent.picking":       "Открыто окно выбора программы Windows — выберите программу агента в нём.",
	"settings.agent.pick_title":    "Выберите программу агента (codex или claude)",
	"settings.agent.filter":        "Программы (*.exe;*.cmd;*.bat)",
	"settings.agent.cancelled":     "Отменено — программа агента не изменилась.",
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
	"settings.listen.hint":         "Пусто — все сети этого компьютера (ZeroTier, локальная, внешний адрес), порт 7420. Можно указать один IP, например адрес ZeroTier.",
	"settings.discovery.label":     "Искать участников в локальной сети и ZeroTier",
	"settings.discovery.hint":      "Раз в 5 секунд agentlink рассылает по сетям короткий сигнал (UDP, порт 7421) без кода связи; участники с тем же кодом подключаются друг к другу сами.",
	"settings.api.label":           "Адрес этой страницы",
	"settings.api.hint":            "Пусто — 127.0.0.1:7520; меняется после перезапуска agentlink.",
	"settings.areas.label":         "Общие темы",
	"settings.areas.hint":          "Через запятую: dev, design. Тогда сообщение можно послать на area:dev.",
	"settings.max_jobs.label":      "Сколько вопросов агент решает сразу",
	"settings.max_jobs.hint":       "От 1 до 4; пусто — 2. Остальные вопросы ждут в очереди и начинаются по порядку прихода.",
	"settings.autostart.label":     "Запускать agentlink при входе в Windows",
	"settings.save":                "Сохранить",
	"settings.saving":              "Сохраняю…",
	"settings.saved":               "Сохранено.",

	// Updates: the settings page section and the tray menu.
	"update.legend":        "Обновления",
	"update.version":       "Версия {version}",
	"update.check":         "Проверить обновления",
	"update.apply":         "Обновить до {version}",
	"update.auto":          "Обновлять автоматически",
	"update.auto.hint":     "Раз в несколько часов agentlink проверяет выпуски на GitHub и сам ставит новую версию. Вопросы, над которыми работает агент, при перезапуске не прерываются.",
	"update.checking":      "Проверяю обновления…",
	"update.latest":        "У вас актуальная версия.",
	"update.available":     "Доступна версия {version}.",
	"update.none":          "На GitHub пока нет выпуска для этой системы.",
	"update.applying":      "Скачиваю и проверяю версию {version}…",
	"update.restarting":    "Версия {version} установлена — agentlink перезапускается, агент продолжает работу.",
	"update.reload":        "agentlink перезапускается — обновите страницу (F5) через несколько секунд.",
	"update.disabled":      "Эта сборка без номера версии — обновление отключено.",
	"update.error.check":   "Не удалось проверить обновления — проверьте интернет; подробности в файле agentlink.log.",
	"update.error.apply":   "Не удалось установить обновление, осталась текущая версия — подробности в файле agentlink.log.",
	"update.error.restart": "Обновление установлено, но agentlink не перезапустился — закройте его и запустите снова.",

	// Errors: each one sentence saying what to do.
	"error.node":                   "Впишите ваше имя: буквы, цифры, «-» или «_», без пробелов.",
	"error.code":                   "Код связи — 12 латинских букв и цифр вида XXXX-XXXX-XXXX (или старый код из 6): нажмите «Создать код» или впишите код, который вам дали.",
	"error.peer_addr":              "Адрес участника не похож на IP — впишите, например, 10.147.20.9 или 10.147.20.9:7420.",
	"error.work_dir":               "Укажите существующую рабочую папку, в которой агент будет искать ответы, или выберите «Никто, отвечаю сам».",
	"error.handler":                "Выберите, кто отвечает: Claude Code, Codex или никто.",
	"error.handler_missing":        "Программа агента не найдена ни в одном известном месте — установите агента (приложение Codex, Claude Code, npm) или нажмите «Указать…» и выберите codex.exe / claude.exe.",
	"error.agent_path":             "«Программа агента» не найдена по указанному пути — нажмите «Указать…» и выберите её заново.",
	"error.listen_format":          "«Мой адрес» в «Дополнительно» должен быть IP этого компьютера, например 10.147.20.5, или пустым (все сети).",
	"error.listen":                 "Не удалось занять адрес {addr} — проверьте «Мой адрес» в «Дополнительно» или закройте другую копию agentlink.",
	"error.api":                    "«Адрес этой страницы» в «Дополнительно» должен быть вида 127.0.0.1:7520 или пустым.",
	"error.areas":                  "Общие темы в «Дополнительно» — слова из букв и цифр через запятую, например dev, design.",
	"error.peer_name":              "Имя участника в файле настроек — буквы, цифры, «-» или «_»; уберите его или исправьте.",
	"error.max_jobs":               "«Сколько вопросов агент решает сразу» в «Дополнительно» — целое число от 1 до 4 или пусто.",
	"error.peer_name_self":         "Имя одного из участников в файле настроек совпадает с вашим — поменяйте ваше имя.",
	"error.peer_name_twice":        "Одно имя участника записано в файле настроек дважды — оставьте его у одного адреса.",
	"error.not_configured":         "Сначала заполните настройки и нажмите «Сохранить», потом добавляйте участников.",
	"error.remove_self":            "Себя удалить нельзя — удалить вас может другой участник.",
	"error.unknown_member":         "Такого участника нет — обновите страницу (F5).",
	"error.ambiguous_peer":         "Участников несколько — впишите в «Кому» имя: {peers}.",
	"error.start":                  "Настройки сохранены, но agentlink не запустился — подробности в файле agentlink.log рядом с настройками.",
	"error.save":                   "Не удалось записать настройки на диск — проверьте, что папка %APPDATA%\\agentlink доступна, и попробуйте снова.",
	"error.bad_request":            "Страница прислала неверные данные — обновите её (F5) и попробуйте снова.",
	"error.forbidden":              "Страница устарела — обновите её (F5).",
	"error.no_app":                 "agentlink не отвечает — запустите agentlink.exe и обновите страницу.",
	"error.internal":               "Что-то пошло не так — подробности в файле agentlink.log рядом с настройками.",
	"error.not_running":            "agentlink ещё не подключён — впишите код связи в настройках и сохраните.",
	"error.empty_body":             "Напишите текст сообщения.",
	"error.unknown_peer":           "Такой участник ещё ни разу не подключался — проверьте имя или дождитесь надписи «на связи» и отправьте снова.",
	"error.no_area_peer":           "На эту тему никто не подписан — проверьте «Общие темы» у участников.",
	"error.send":                   "Не удалось отправить сообщение — подробности в файле agentlink.log рядом с настройками.",
	"error.pick_busy":              "Окно выбора Windows уже открыто — закончите выбор в нём или закройте его.",
	"error.pick_unsupported":       "Окно выбора папки есть только в Windows — впишите путь к папке вручную.",
	"error.pick":                   "Не удалось открыть окно выбора папки — впишите путь вручную; подробности в файле agentlink.log.",
	"error.pick_agent_unsupported": "Окно выбора программы есть только в Windows — поставьте агента так, чтобы он был в PATH.",
	"error.pick_agent":             "Не удалось открыть окно выбора программы — подробности в файле agentlink.log.",
	// Inbox page: the send form.
	"inbox.h1":           "Сообщения",
	"inbox.send.legend":  "Новый вопрос",
	"inbox.to.label":     "Кому",
	"inbox.to.hint":      "Имя участника; пусто — если участник один; area:тема — рассылка по теме.",
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
	"inbox.activity":  "сейчас: {activity}",
	"inbox.no_news":   "нет вестей от участника {min} мин",

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

// Text is msg for the tray menu.
func Text(key string, vars map[string]string) string { return msg(key, vars) }

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
