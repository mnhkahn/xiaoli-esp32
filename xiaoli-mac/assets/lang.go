// Package assets holds the localized status strings, mirroring
// xiaozhi-esp32/main/assets/locales/zh-CN/language.json.
package assets

// Status is the i18n key for a state label.
type Status string

const (
	StatusStandby  Status = "STANDBY"
	StatusListening Status = "LISTENING"
	StatusSpeaking Status = "SPEAKING"
)

// Locale returns the display string for a given language and status.
// Falls back to zh-CN if the language is unknown.
func Locale(lang, key string) string {
	if table, ok := locales[lang]; ok {
		if v, ok := table[key]; ok {
			return v
		}
	}
	if v, ok := locales["zh-CN"][key]; ok {
		return v
	}
	return key
}

// locales mirrors the C++ Lang::Strings table. Add new keys here when
// extending the display interface.
var locales = map[string]map[string]string{
	"zh-CN": {
		"STANDBY":   "待命",
		"LISTENING": "聆听中...",
		"SPEAKING":  "说话中...",
		"CONNECTING": "连接中...",
		"UPGRADING":  "升级中...",
		"ACTIVATING": "激活中...",
	},
	"zh-TW": {
		"STANDBY": "待命",
	},
	"en-US": {
		"STANDBY":   "Standby",
		"LISTENING": "Listening...",
		"SPEAKING":  "Speaking...",
		"CONNECTING": "Connecting...",
		"UPGRADING":  "Upgrading...",
		"ACTIVATING": "Activating...",
	},
}
