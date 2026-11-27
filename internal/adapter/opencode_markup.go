package adapter

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

var openCodeMarkdown = goldmark.DefaultParser()

// protectedOpenCodeMarker checks the position in the original model output.
// Parsing the whole frame preserves the actual containers and the delimiters
// on both sides of the marker, including multiline code spans.
func protectedOpenCodeMarker(reasoning string, marker int) bool {
	before, after := reasoning[:marker], reasoning[marker+len(deepSeekThinkingEnd):]
	if openCodeInlineQuote(openCodeParagraphPrefix(before), after) {
		return true
	}
	return walkOpenCodeLiterals(reasoning, func(segment text.Segment, _ bool) bool {
		return segment.Start <= marker && marker+len(deepSeekThinkingEnd) <= segment.Stop
	})
}

// openCodeHasActiveThinkingTag allows literal examples in Markdown code. A
// second tag outside code is ambiguous, including one in ordinary HTML text.
func openCodeHasActiveThinkingTag(answer string) bool {
	if !strings.Contains(answer, deepSeekThinkingStart) && !strings.Contains(answer, deepSeekThinkingEnd) {
		return false
	}
	masked := []byte(answer)
	walkOpenCodeLiterals(answer, func(segment text.Segment, code bool) bool {
		if code {
			for _, marker := range []string{deepSeekThinkingStart, deepSeekThinkingEnd} {
				region := masked[segment.Start:segment.Stop]
				for offset := bytes.Index(region, []byte(marker)); offset >= 0; offset = bytes.Index(region, []byte(marker)) {
					region[offset] = 0
					region = region[offset+len(marker):]
				}
			}
		}
		return false
	})
	return bytes.Contains(masked, []byte(deepSeekThinkingStart)) || bytes.Contains(masked, []byte(deepSeekThinkingEnd))
}

// walkOpenCodeLiterals stops when the visitor accepts a segment. The code
// argument distinguishes Markdown code from HTML and blockquote text.
func walkOpenCodeLiterals(source string, visit func(segment text.Segment, code bool) bool) bool {
	visitLines := func(segments *text.Segments, code bool) bool {
		for i := range segments.Len() {
			if visit(segments.At(i), code) {
				return true
			}
		}
		return false
	}
	document := openCodeMarkdown.Parse(text.NewReader([]byte(source)))
	found := false
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.CodeBlock:
			found = visitLines(n.Lines(), true)
		case *ast.FencedCodeBlock:
			found = visitLines(n.Lines(), true) || n.Info != nil && visit(n.Info.Segment, false)
		case *ast.Text:
			found = n.Parent().Kind() == ast.KindCodeSpan && visit(n.Segment, true)
		case *ast.HTMLBlock:
			found = visitLines(n.Lines(), false) || n.HasClosure() && visit(n.ClosureLine, false)
		case *ast.RawHTML:
			for i := range n.Segments.Len() {
				segment := n.Segments.At(i)
				raw := source[segment.Start:segment.Stop]
				if raw != deepSeekThinkingStart && raw != deepSeekThinkingEnd && visit(segment, false) {
					found = true
					break
				}
			}
		}
		if !found && node.Type() == ast.TypeBlock && node.Lines().Len() > 0 {
			for parent := node.Parent(); parent != nil; parent = parent.Parent() {
				if parent.Kind() == ast.KindBlockquote {
					found = visitLines(node.Lines(), false)
					break
				}
			}
		}
		if found {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found
}

func openCodeParagraphPrefix(before string) string {
	end := strings.LastIndexByte(before, '\n')
	for end >= 0 {
		start := strings.LastIndexByte(before[:end], '\n') + 1
		if strings.TrimSpace(before[start:end]) == "" {
			return before[end+1:]
		}
		end = start - 1
	}
	return before
}

// Prose quotes can span lines within a paragraph. Markdown handles multiline
// code spans separately, so a stray backtick cannot affect quote parsing on
// the following line.
func openCodeInlineQuote(line, suffix string) bool {
	lineSuffix, _, _ := strings.Cut(suffix, "\n")
	hasClosingSingleQuote := strings.ContainsAny(lineSuffix, "'’")
	var quote, closeQuote rune
	var quoteDepth int
	var codeWidth int
	for next := 0; next < len(line); {
		i := next
		c, width := utf8.DecodeRuneInString(line[i:])
		next = i + width
		switch {
		case c == '\n':
			codeWidth = 0
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
		case unicode.Is(unicode.Quotation_Mark, c):
			inWord := i > 0 && next < len(line) && openCodeWordByte(line[i-1]) && openCodeWordByte(line[next])
			apostrophe := c == '\'' || c == '’'
			if apostrophe && (inWord || quote == 0 && !hasClosingSingleQuote && openCodeDecade(line[next:])) {
				continue
			}
			switch {
			case quote == 0:
				if !strings.ContainsRune("'\"’”", c) || openCodeBeforeQuote(line, i) {
					quote, closeQuote, quoteDepth = c, openCodeQuoteEnd(c), 1
				}
			case c == closeQuote || quote == '〝' && c == '〟':
				quoteDepth--
				if quoteDepth == 0 {
					quote = 0
				}
			case c == quote:
				quoteDepth++
			}
		}
	}
	return quote != 0 || codeWidth != 0 || strings.HasSuffix(line, "\\")
}

func openCodeQuoteEnd(open rune) rune {
	switch open {
	case '‘':
		return '’'
	case '“':
		return '”'
	case '‚':
		return '‘'
	case '„':
		return '“'
	case '«':
		return '»'
	case '»':
		return '«'
	case '‹':
		return '›'
	case '›':
		return '‹'
	case '「', '﹁', '｢':
		return open + 1
	case '『', '﹃':
		return open + 1
	case '〝':
		return '〞'
	default:
		return open
	}
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
