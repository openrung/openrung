package directsetup

import "strings"

func conciseOutput(output []byte) string {
	value := strings.TrimSpace(string(output))
	if value == "" {
		return ""
	}
	if len(value) > 240 {
		value = value[:240] + "…"
	}
	return " " + value
}
