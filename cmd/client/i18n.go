package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/openrung/openrung/connectcore"
)

// The TUI ships English, Chinese, and Russian. There is no settings entry for
// this: the . key cycles the language directly, and the footer help token that
// advertises it stays in all three languages at once (languageKeyHelp) so a
// reader can always find their way back regardless of the current language.
//
// The platform TUN copy (tunModeSummary/tunModeAdvice) and everything the
// engine emits — log lines, error strings, notice reasons — stay English:
// they quote flags, commands, and sing-box output that only exist in English.

type language int

const (
	langEnglish language = iota
	langChinese
	langRussian
	languageCount
)

// languageKeyHelp is identical in every language on purpose: the switch must
// be recognizable before the current language is readable, so every footer
// carries this same trilingual token. The key is punctuation for the matching
// reason — see the "." case in handleKey.
const languageKeyHelp = ". lang/语言/язык"

type translation struct {
	tabs [viewCount]string

	helpGlobal       string
	helpRelays       string
	helpLogs         string
	helpSettings     string
	helpSettingsEdit string

	labelStatus    string
	labelRelay     string
	labelCountry   string
	labelTransport string
	labelSession   string
	labelHealth    string
	labelActivity  string
	labelCapture   string
	labelProxy     string
	labelBroker    string
	labelTarget    string
	labelError     string

	statusNames map[connectcore.Status]string

	captureTUN     string
	captureProxy   string
	proxyResolving string
	defaultFronts  string

	transportDirect  string
	transportPunched string
	transportWSS     string // fmt: front id

	healthProbing string
	healthOK      string
	healthFailed  string // fmt: failures, threshold

	noticeFailoverStarted   string // fmt: relay id, reason
	noticeFailoverCompleted string // fmt: from relay, to relay, reason
	noticeViaFront          string // fmt: front id
	noticeWSSFallback       string // fmt: relay id, front id, reason
	noticeTicketRetry       string // fmt: front id, wait
	noticePunch             string // fmt: relay id, reason

	targetRelay     string // fmt: relay id
	targetLabel     string // fmt: label
	targetCountry   string // fmt: country
	targetAutomatic string

	refreshingDirectory string
	directoryErrPrefix  string
	noRelaysYet         string
	colRelay            string
	colCountry          string
	colLatency          string
	colClass            string
	targetMarker        string
	badgeFoundation     string
	badgeVolunteer      string

	noLogOutput string

	fieldNames              [settingsFieldCount]string
	enableInShell           string
	restoreShell            string
	restoreAdvice           string
	availableWhileConnected string
	notNeededTUN            string
	commandsBelow           string
	pressEnterShell         string
	modeTUN                 string // fmt: platform TUN summary
	modeProxy               string
	modeNoteTUN             string // fmt: platform TUN advice
	modeNoteProxy           string

	noteShellTUN          string
	noteShellDisconnected string
	shellUnavailable      string
}

var translations = [languageCount]*translation{
	langEnglish: {
		tabs: [viewCount]string{"1 Status", "2 Relays", "3 Logs", "4 Settings"},

		helpGlobal:       "c connect · d disconnect · r refresh · " + languageKeyHelp + " · q quit",
		helpRelays:       "↑/↓ select · enter connect to selection · x clear target · ",
		helpLogs:         "↑/↓/pgup/pgdn scroll · ",
		helpSettings:     "↑/↓ field · enter edit · ",
		helpSettingsEdit: "enter apply · esc cancel",

		labelStatus:    "Status",
		labelRelay:     "Relay",
		labelCountry:   "Country",
		labelTransport: "Transport",
		labelSession:   "Session",
		labelHealth:    "Health",
		labelActivity:  "Activity",
		labelCapture:   "Capture",
		labelProxy:     "Proxy",
		labelBroker:    "Broker",
		labelTarget:    "Target",
		labelError:     "Error",

		statusNames: map[connectcore.Status]string{
			connectcore.StatusDisconnected:  "disconnected",
			connectcore.StatusPreparing:     "preparing",
			connectcore.StatusConnecting:    "connecting",
			connectcore.StatusConnected:     "connected",
			connectcore.StatusDisconnecting: "disconnecting",
			connectcore.StatusFailed:        "failed",
		},

		captureTUN:     "TUN — whole device",
		captureProxy:   "proxy — applications configured for the endpoint below",
		proxyResolving: "resolving…",
		defaultFronts:  "default fronts",

		transportDirect:  "direct",
		transportPunched: "punched (direct NAT path)",
		transportWSS:     "WSS front %s",

		healthProbing: "probing every 30s…",
		healthOK:      "ok",
		healthFailed:  "%d/%d probes failed",

		noticeFailoverStarted:   "failover: relay %s lost (%s); re-laddering",
		noticeFailoverCompleted: "failover: relay %s → %s (%s)",
		noticeViaFront:          " via WSS front %s",
		noticeWSSFallback:       "WSS fallback: relay %s via front %s (direct path: %s)",
		noticeTicketRetry:       "WSS tickets rate-limited; retrying front %s in %s",
		noticePunch:             "punch %s: %s",

		targetRelay:     "relay %s",
		targetLabel:     "label %s",
		targetCountry:   "country %s",
		targetAutomatic: "automatic (ranked)",

		refreshingDirectory: "refreshing relay directory…",
		directoryErrPrefix:  "directory: ",
		noRelaysYet:         "no relays yet — press r to refresh",
		colRelay:            "RELAY",
		colCountry:          "COUNTRY",
		colLatency:          "LATENCY",
		colClass:            "CLASS",
		targetMarker:        "← target",
		badgeFoundation:     "[foundation]",
		badgeVolunteer:      "[volunteer]",

		noLogOutput: "no log output yet",

		fieldNames: [settingsFieldCount]string{
			"Broker URL", "Mode", "Shell proxy",
		},
		enableInShell:           "Enable in a shell",
		restoreShell:            "Restore that shell",
		restoreAdvice:           "run the restore command after disconnect, failure, quit, or crash",
		availableWhileConnected: "available while connected",
		notNeededTUN:            "not needed in TUN mode",
		commandsBelow:           "commands below",
		pressEnterShell:         "press enter to show the shell commands",
		modeTUN:                 "TUN — whole device (%s)",
		modeProxy:               "proxy — local mixed HTTP/SOCKS inbound (no privileges)",
		modeNoteTUN:             "TUN mode: the next connect captures every application — %s",
		modeNoteProxy:           "proxy mode: the next connect serves a local mixed proxy and points the system proxy at it",

		noteShellTUN:          "TUN mode already routes every application; the shell proxy is a proxy-mode helper",
		noteShellDisconnected: "connect first — the shell proxy only works while connected",
		shellUnavailable:      "shell integration is unavailable in this build",
	},

	langChinese: {
		tabs: [viewCount]string{"1 状态", "2 中继", "3 日志", "4 设置"},

		helpGlobal:       "c 连接 · d 断开 · r 刷新 · " + languageKeyHelp + " · q 退出",
		helpRelays:       "↑/↓ 选择 · enter 连接所选 · x 清除目标 · ",
		helpLogs:         "↑/↓/pgup/pgdn 滚动 · ",
		helpSettings:     "↑/↓ 字段 · enter 编辑 · ",
		helpSettingsEdit: "enter 应用 · esc 取消",

		labelStatus:    "状态",
		labelRelay:     "中继",
		labelCountry:   "国家",
		labelTransport: "传输",
		labelSession:   "会话",
		labelHealth:    "健康",
		labelActivity:  "活动",
		labelCapture:   "捕获",
		labelProxy:     "代理",
		labelBroker:    "调度服务器",
		labelTarget:    "目标",
		labelError:     "错误",

		statusNames: map[connectcore.Status]string{
			connectcore.StatusDisconnected:  "未连接",
			connectcore.StatusPreparing:     "准备中",
			connectcore.StatusConnecting:    "连接中",
			connectcore.StatusConnected:     "已连接",
			connectcore.StatusDisconnecting: "断开中",
			connectcore.StatusFailed:        "失败",
		},

		captureTUN:     "TUN — 全设备接管",
		captureProxy:   "代理 — 应用需配置为下方端点",
		proxyResolving: "解析中…",
		defaultFronts:  "默认前置",

		transportDirect:  "直连",
		transportPunched: "打洞（NAT 直连路径）",
		transportWSS:     "WSS 前置 %s",

		healthProbing: "每 30 秒探测中…",
		healthOK:      "正常",
		healthFailed:  "%d/%d 次探测失败",

		noticeFailoverStarted:   "故障转移：中继 %s 失联（%s）；正在重新选路",
		noticeFailoverCompleted: "故障转移：中继 %s → %s（%s）",
		noticeViaFront:          "，经 WSS 前置 %s",
		noticeWSSFallback:       "WSS 回退：中继 %s 经前置 %s（直连路径：%s）",
		noticeTicketRetry:       "WSS 票据被限流；将在 %[2]s 后重试前置 %[1]s",
		noticePunch:             "打洞 %s：%s",

		targetRelay:     "中继 %s",
		targetLabel:     "标签 %s",
		targetCountry:   "国家 %s",
		targetAutomatic: "自动（按排名）",

		refreshingDirectory: "正在刷新中继目录…",
		directoryErrPrefix:  "目录：",
		noRelaysYet:         "暂无中继 — 按 r 刷新",
		colRelay:            "中继",
		colCountry:          "国家",
		colLatency:          "延迟",
		colClass:            "类型",
		targetMarker:        "← 目标",
		badgeFoundation:     "[官方]",
		badgeVolunteer:      "[志愿]",

		noLogOutput: "暂无日志输出",

		fieldNames: [settingsFieldCount]string{
			"Broker 地址", "模式", "Shell 代理",
		},
		enableInShell:           "在 shell 中启用",
		restoreShell:            "恢复该 shell",
		restoreAdvice:           "断开、失败、退出或崩溃后，请运行恢复命令",
		availableWhileConnected: "连接后可用",
		notNeededTUN:            "TUN 模式下无需",
		commandsBelow:           "命令见下方",
		pressEnterShell:         "按 enter 显示 shell 命令",
		modeTUN:                 "TUN — 全设备接管（%s）",
		modeProxy:               "代理 — 本地混合 HTTP/SOCKS 入站（无需特权）",
		modeNoteTUN:             "TUN 模式：下次连接将接管所有应用 — %s",
		modeNoteProxy:           "代理模式：下次连接将启动本地混合代理，并将系统代理指向它",

		noteShellTUN:          "TUN 模式已接管所有应用；shell 代理仅用于代理模式",
		noteShellDisconnected: "请先连接 — shell 代理仅在已连接时可用",
		shellUnavailable:      "此构建不支持 shell 集成",
	},

	langRussian: {
		tabs: [viewCount]string{"1 Статус", "2 Узлы", "3 Журнал", "4 Настройки"},

		helpGlobal:       "c подключить · d отключить · r обновить · " + languageKeyHelp + " · q выход",
		helpRelays:       "↑/↓ выбор · enter подключиться к выбранному · x сбросить цель · ",
		helpLogs:         "↑/↓/pgup/pgdn прокрутка · ",
		helpSettings:     "↑/↓ поле · enter изменить · ",
		helpSettingsEdit: "enter применить · esc отмена",

		labelStatus:    "Статус",
		labelRelay:     "Узел",
		labelCountry:   "Страна",
		labelTransport: "Транспорт",
		labelSession:   "Сессия",
		labelHealth:    "Проверка",
		labelActivity:  "События",
		labelCapture:   "Захват",
		labelProxy:     "Прокси",
		labelBroker:    "Брокер",
		labelTarget:    "Цель",
		labelError:     "Ошибка",

		statusNames: map[connectcore.Status]string{
			connectcore.StatusDisconnected:  "не подключено",
			connectcore.StatusPreparing:     "подготовка",
			connectcore.StatusConnecting:    "подключение",
			connectcore.StatusConnected:     "подключено",
			connectcore.StatusDisconnecting: "отключение",
			connectcore.StatusFailed:        "сбой",
		},

		captureTUN:     "TUN — всё устройство",
		captureProxy:   "прокси — приложения настраиваются на адрес ниже",
		proxyResolving: "определение…",
		defaultFronts:  "фронты по умолчанию",

		transportDirect:  "напрямую",
		transportPunched: "пробито (прямой путь через NAT)",
		transportWSS:     "WSS-фронт %s",

		healthProbing: "проверка каждые 30 с…",
		healthOK:      "ок",
		healthFailed:  "%d/%d проверок не прошло",

		noticeFailoverStarted:   "переключение: узел %s потерян (%s); повторный выбор пути",
		noticeFailoverCompleted: "переключение: узел %s → %s (%s)",
		noticeViaFront:          " через WSS-фронт %s",
		noticeWSSFallback:       "резерв WSS: узел %s через фронт %s (прямой путь: %s)",
		noticeTicketRetry:       "билеты WSS ограничены; повтор фронта %s через %s",
		noticePunch:             "пробивка %s: %s",

		targetRelay:     "узел %s",
		targetLabel:     "метка %s",
		targetCountry:   "страна %s",
		targetAutomatic: "автоматически (по рейтингу)",

		refreshingDirectory: "обновление каталога узлов…",
		directoryErrPrefix:  "каталог: ",
		noRelaysYet:         "узлов пока нет — нажмите r для обновления",
		colRelay:            "УЗЕЛ",
		colCountry:          "СТРАНА",
		colLatency:          "ЗАДЕРЖКА",
		colClass:            "КЛАСС",
		targetMarker:        "← цель",
		badgeFoundation:     "[фонд]",
		badgeVolunteer:      "[волонтёр]",

		noLogOutput: "журнал пока пуст",

		fieldNames: [settingsFieldCount]string{
			"URL брокера", "Режим", "Прокси для shell",
		},
		enableInShell:           "Включить в shell",
		restoreShell:            "Восстановить shell",
		restoreAdvice:           "выполните команду восстановления после отключения, сбоя, выхода или падения",
		availableWhileConnected: "доступно при подключении",
		notNeededTUN:            "не нужно в режиме TUN",
		commandsBelow:           "команды ниже",
		pressEnterShell:         "нажмите enter, чтобы показать команды shell",
		modeTUN:                 "TUN — всё устройство (%s)",
		modeProxy:               "прокси — локальный смешанный HTTP/SOCKS-вход (без привилегий)",
		modeNoteTUN:             "режим TUN: следующее подключение захватит все приложения — %s",
		modeNoteProxy:           "режим прокси: следующее подключение запустит локальный прокси и направит на него системный прокси",

		noteShellTUN:          "режим TUN уже маршрутизирует все приложения; прокси для shell — помощник режима прокси",
		noteShellDisconnected: "сначала подключитесь — прокси для shell работает только при подключении",
		shellUnavailable:      "интеграция с shell недоступна в этой сборке",
	},
}

// tr resolves the active language's string table. The zero model value is
// English, so views rendered before any key press match the old copy exactly.
func (m tuiModel) tr() *translation {
	if m.lang < 0 || m.lang >= languageCount {
		return translations[langEnglish]
	}
	return translations[m.lang]
}

func (tr *translation) statusName(status connectcore.Status) string {
	if name, ok := tr.statusNames[status]; ok {
		return name
	}
	return string(status)
}

// padCell pads to display width, not byte count: fmt's %-8s would leave CJK
// and Cyrillic table headers misaligned against their ASCII rows.
func padCell(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
