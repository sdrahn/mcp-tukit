package server

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// What callers send ends up in the journal and in snapshot descriptions.
// Neither may be shaped by it: a newline in a command word would start a
// log line of its own, which reads as the server's.

// maxLogLine bounds a log line, maxDescription a snapshot's description.
const (
	maxLogLine     = 2048
	maxDescription = 200
)

// printable returns s on one line, with control characters and the
// Unicode line and paragraph separators escaped, and invalid UTF-8
// replaced.
func printable(s string) string {
	clean := true
	for _, r := range s {
		if unsafeRune(r) {
			clean = false
			break
		}
	}
	if clean && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "\uFFFD") {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case unsafeRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}

// shorten cuts s to at most max bytes, at a rune boundary, marking the
// cut.
func shorten(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "…"
	cut := max - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// description is the description of a snapshot made for what.
func description(what string) string {
	return shorten("MCP: "+printable(what), maxDescription)
}

// checkDescription refuses a caller's snapshot description that is too
// long or would not print on one line.
func checkDescription(d string) error {
	if len(d) > maxDescription-len("MCP: ") {
		return fmt.Errorf("description: longer than %d bytes", maxDescription-len("MCP: "))
	}
	if d != printable(d) {
		return fmt.Errorf("description: control characters are not allowed")
	}
	return nil
}
