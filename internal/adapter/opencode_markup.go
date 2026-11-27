package adapter

import "strings"

// protectedOpenCodeMarker detects a marker that is being quoted in reasoning,
// including fenced examples. The final line ends immediately before the marker,
// so it cannot close a fence that requires the rest of the line to be blank.
func protectedOpenCodeMarker(prefix string) bool {
	var fence byte
	var fenceWidth int
	for {
		line, rest, complete := strings.Cut(prefix, "\n")
		kind, width, tail := openCodeFencePrefix(line)
		switch {
		case fence != 0:
			if complete && kind == fence && width >= fenceWidth && strings.TrimSpace(tail) == "" {
				fence, fenceWidth = 0, 0
			}
		case kind != 0:
			fence, fenceWidth = kind, width
		}
		if !complete {
			return fence != 0 || openCodeInlineQuote(line)
		}
		prefix = rest
	}
}

func openCodeFencePrefix(line string) (kind byte, width int, tail string) {
	line = strings.TrimLeft(line, " \t")
	for strings.HasPrefix(line, ">") {
		line = strings.TrimLeft(line[1:], " \t")
	}
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0, ""
	}
	width = openCodeRunWidth(line, line[0])
	if width < 3 {
		return 0, 0, ""
	}
	return line[0], width, line[width:]
}

// Inline prose quoting is local to the marker line. An unbalanced apostrophe
// or backtick in an earlier line cannot invalidate a later completed turn.
func openCodeInlineQuote(line string) bool {
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
			if c == '\'' && inWord {
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
