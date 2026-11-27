package adapter

import "strings"

// protectedOpenCodeMarker detects a marker that is being quoted in reasoning,
// including fenced examples. The final line ends immediately before the marker,
// so it cannot close a fence that requires the rest of the line to be blank.
func protectedOpenCodeMarker(prefix, suffix string) bool {
	var fence byte
	var fenceWidth int
	for {
		line, rest, complete := strings.Cut(prefix, "\n")
		kind, width, tail, listItem := openCodeFencePrefix(line)
		switch {
		case fence != 0:
			if complete && !listItem && kind == fence && width >= fenceWidth && strings.TrimSpace(tail) == "" {
				fence, fenceWidth = 0, 0
			}
		case kind != 0 && (kind != '`' || !strings.Contains(tail, "`")):
			// A backtick fence cannot contain backticks in its info string.
			fence, fenceWidth = kind, width
		}
		if !complete {
			trimmed := strings.TrimLeft(line, " \t")
			indent := line[:len(line)-len(trimmed)]
			return fence != 0 || strings.HasPrefix(trimmed, ">") || len(indent) >= 4 ||
				strings.Contains(indent, "\t") || openCodeInlineQuote(line, suffix)
		}
		prefix = rest
	}
}

func openCodeFencePrefix(line string) (kind byte, width int, tail string, listItem bool) {
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
		listItem = true
		line = strings.TrimLeft(line[marker:], " \t")
	}
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0, "", listItem
	}
	width = openCodeRunWidth(line, line[0])
	if width < 3 {
		return 0, 0, "", listItem
	}
	return line[0], width, line[width:], listItem
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
	line = openCodeSmartQuotes.Replace(line)
	lineSuffix, _, _ := strings.Cut(suffix, "\n")
	hasClosingSingleQuote := strings.ContainsAny(lineSuffix, "'’")
	var quote byte
	var codeWidth int
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '`' && quote == 0:
			width := openCodeRunWidth(line[i:], c)
			if codeWidth == 0 {
				codeWidth = width
			} else if width == codeWidth {
				codeWidth = 0
			}
			i += width - 1
		case codeWidth != 0:
			// Quotes in code spans are literal.
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			inWord := i > 0 && i+1 < len(line) && openCodeWordByte(line[i-1]) && openCodeWordByte(line[i+1])
			if c == '\'' && (inWord || quote == 0 && !hasClosingSingleQuote && openCodeDecade(line[i+1:])) {
				continue
			}
			if quote == c {
				quote = 0
			} else if quote == 0 && (i == 0 || (!openCodeWordByte(line[i-1]) && line[i-1] != '`')) {
				quote = c
			}
		}
	}
	return quote != 0 || codeWidth != 0 || strings.HasSuffix(line, "\\")
}

var openCodeSmartQuotes = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`)

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
