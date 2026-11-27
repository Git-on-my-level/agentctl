package adapter

import (
	"strings"
	"unicode/utf8"
)

// protectedOpenCodeMarker detects a marker that is being quoted in reasoning,
// including fenced examples. The final line ends immediately before the marker,
// so it cannot close a fence that requires the rest of the line to be blank.
func protectedOpenCodeMarker(prefix, suffix string) bool {
	var fence byte
	var fenceWidth, listIndent int
	for {
		line, rest, complete := strings.Cut(prefix, "\n")
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		kind, width, tail, itemIndent := openCodeFencePrefix(line)
		if fence == 0 && trimmed != "" {
			if indent < listIndent {
				listIndent = 0
			}
			if itemIndent > 0 && indent-listIndent < 4 {
				listIndent = itemIndent
			}
		}
		switch {
		case fence != 0:
			if complete && itemIndent == 0 && indent >= listIndent && indent-listIndent < 4 && kind == fence && width >= fenceWidth && strings.TrimSpace(tail) == "" {
				fence, fenceWidth = 0, 0
			}
		case kind != 0 && (kind != '`' || !strings.Contains(tail, "`")):
			// A backtick fence cannot contain backticks in its info string.
			fence, fenceWidth = kind, width
		}
		if !complete {
			return fence != 0 || strings.HasPrefix(trimmed, ">") || indent-listIndent >= 4 ||
				strings.Contains(line[:indent], "\t") || openCodeInlineQuote(line, suffix)
		}
		prefix = rest
	}
}

func openCodeFencePrefix(line string) (kind byte, width int, tail string, itemIndent int) {
	remaining := len(line)
	line = strings.TrimLeft(line, " \t")
	for line != "" {
		if line[0] == '>' {
			line = strings.TrimLeft(line[1:], " \t")
			continue
		}
		marker := openCodeListMarker(line)
		if marker == 0 {
			break
		}
		line = strings.TrimLeft(line[marker:], " \t")
		itemIndent = remaining - len(line)
	}
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0, "", itemIndent
	}
	width = openCodeRunWidth(line, line[0])
	if width < 3 {
		return 0, 0, "", itemIndent
	}
	return line[0], width, line[width:], itemIndent
}

// Markdown allows a list item's first block to start directly after its marker.
func openCodeListMarker(line string) int {
	marker := 0
	if strings.ContainsRune("-+*", rune(line[0])) {
		marker = 1
	} else {
		digits := 0
		for digits < len(line) && digits < 9 && line[digits] >= '0' && line[digits] <= '9' {
			digits++
		}
		if digits > 0 && digits < len(line) && (line[digits] == '.' || line[digits] == ')') {
			marker = digits + 1
		}
	}
	if marker < len(line) && (line[marker] == ' ' || line[marker] == '\t') {
		return marker
	}
	return 0
}

// Inline prose quoting is local to the marker line. An unbalanced apostrophe
// or backtick in an earlier line cannot invalidate a later completed turn.
func openCodeInlineQuote(line, suffix string) bool {
	lineSuffix, _, _ := strings.Cut(suffix, "\n")
	hasClosingSingleQuote := strings.ContainsAny(lineSuffix, "'’")
	var quote rune
	var codeWidth int
	for next := 0; next < len(line); {
		i := next
		c, width := utf8.DecodeRuneInString(line[i:])
		next = i + width
		switch {
		case c == '`' && quote == 0:
			width := openCodeRunWidth(line[i:], '`')
			if codeWidth == 0 {
				codeWidth = width
			} else if width == codeWidth {
				codeWidth = 0
			}
			next = i + width
		case codeWidth != 0:
			// Quotes in code spans are literal.
		case c == '\\':
			_, escaped := utf8.DecodeRuneInString(line[next:])
			next += escaped
		case c == '\'' || c == '‘' || c == '’':
			inWord := i > 0 && next < len(line) && openCodeWordByte(line[i-1]) && openCodeWordByte(line[next])
			if c != '‘' && (inWord || quote == 0 && !hasClosingSingleQuote && openCodeDecade(line[next:])) {
				continue
			}
			if quote == c {
				quote = 0
			} else if quote == 0 && c == '‘' {
				quote = '’'
			} else if quote == 0 && c == '\'' && openCodeBeforeQuote(line, i) {
				quote = c
			}
		case c == quote:
			quote = 0
		case quote == 0 && c == '“':
			quote = '”'
		case quote == 0 && c == '"' && openCodeBeforeQuote(line, i):
			quote = '"'
		}
	}
	return quote != 0 || codeWidth != 0 || strings.HasSuffix(line, "\\")
}

func openCodeBeforeQuote(line string, index int) bool {
	return index == 0 || (!openCodeWordByte(line[index-1]) && line[index-1] != '`')
}

func openCodeDecade(s string) bool {
	return len(s) >= 3 && s[0] >= '0' && s[0] <= '9' && s[1] >= '0' && s[1] <= '9' &&
		s[2] == 's' && (len(s) == 3 || !openCodeWordByte(s[3]))
}

func openCodeRunWidth(value string, c byte) int {
	i := 0
	for i < len(value) && value[i] == c {
		i++
	}
	return i
}

func openCodeWordByte(c byte) bool {
	return c >= 0x80 || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
