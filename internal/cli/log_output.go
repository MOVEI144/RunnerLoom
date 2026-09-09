package cli

import (
	"strconv"
	"strings"
	"unicode"
)

// Logs are guest-controlled input. Escaping (rather than executing or deleting)
// control sequences preserves evidence without allowing clipboard commands,
// terminal title changes, cursor movement, or bidirectional text spoofing.
func terminalSafeLog(raw []byte) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range string(raw) {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
