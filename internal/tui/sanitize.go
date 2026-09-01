package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// terminalLine makes untrusted graph and query text safe to place in a
// terminal-rendered line. In particular, ESC/C0/C1 controls must never reach
// the terminal: graph properties are durable data and can be written by a
// separate process that does not share the TUI's trust boundary.
func terminalLine(value string) string {
	return sanitizeTerminalText(value, false)
}

func terminalSafeKeyPress(message tea.KeyPressMsg) tea.KeyPressMsg {
	key := tea.Key(message)
	if key.Text == "" {
		return message
	}
	safe := sanitizeTerminalText(key.Text, true)
	if safe == key.Text {
		return message
	}
	key.Text = safe
	if first, _ := utf8.DecodeRuneInString(safe); first != 0 {
		key.Code = first
	}
	key.ShiftedCode = 0
	key.BaseCode = 0
	return tea.KeyPressMsg(key)
}

// terminalBlock is terminalLine's multiline counterpart for Markdown bodies
// and error details. Newlines are retained, while every other terminal control
// is rendered as visible ASCII.
func terminalBlock(value string) string {
	return sanitizeTerminalText(value, true)
}

func sanitizeTerminalText(value string, multiline bool) string {
	value = strings.ToValidUTF8(value, "\\uFFFD")
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		switch character {
		case '\n':
			if multiline {
				result.WriteByte('\n')
			} else {
				result.WriteRune('↵')
			}
		case '\t':
			if multiline {
				result.WriteString("    ")
			} else {
				result.WriteRune('⇥')
			}
		case '\r':
			result.WriteString("\\r")
		default:
			if isTerminalControl(character) {
				switch {
				case character <= 0xff:
					fmt.Fprintf(&result, "\\x%02x", character)
				case character <= 0xffff:
					fmt.Fprintf(&result, "\\u%04x", character)
				default:
					fmt.Fprintf(&result, "\\U%08x", character)
				}
				continue
			}
			result.WriteRune(character)
		}
	}
	return result.String()
}

func isTerminalControl(character rune) bool {
	if character < 0x20 || character == 0x7f || character >= 0x80 && character <= 0x9f {
		return true
	}
	// Bidirectional formatting controls can visually reorder identifiers and
	// operation messages. They have no useful presentation role here.
	return character == 0x061c || character == 0x200e || character == 0x200f ||
		character >= 0x202a && character <= 0x202e ||
		character >= 0x2066 && character <= 0x2069
}

// terminalSafeJSON escapes presentation controls as JSON Unicode escapes.
// Unlike terminalBlock, the result remains valid JSON and decodes to the exact
// original string, which lets edit forms be both safe and lossless.
func terminalSafeJSON(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		if character == '\n' || character == '\r' || character == '\t' || !isTerminalControl(character) {
			result.WriteRune(character)
			continue
		}
		fmt.Fprintf(&result, "\\u%04x", character)
	}
	return result.String()
}

func truncateRunes(value string, limit int) string {
	if limit < 0 {
		return value
	}
	count := 0
	for index := range value {
		if count == limit {
			return value[:index] + "…"
		}
		count++
	}
	return value
}
