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
	document := openCodeMarkdown.Parse(text.NewReader([]byte(reasoning)))
	start := openCodeParagraphStart(document, before)
	var prose []byte
	protected := walkOpenCodeLiterals(reasoning, document, func(segment text.Segment, code bool) bool {
		if segment.Start <= marker && marker+len(deepSeekThinkingEnd) <= segment.Stop {
			return true
		}
		if code && segment.Start < marker && segment.Stop > start {
			if prose == nil {
				prose = []byte(before[start:])
			}
			for i := max(segment.Start, start); i < min(segment.Stop, marker); i++ {
				if prose[i-start] != '\n' {
					prose[i-start] = ' '
				}
			}
		}
		return false
	})
	if protected {
		return true
	}
	if prose != nil {
		return openCodeInlineQuote(string(prose), after)
	}
	return openCodeInlineQuote(before[start:], after)
}

// openCodeHasActiveThinkingTag allows literal examples in Markdown code. A
// second tag outside code is ambiguous, including one in ordinary HTML text.
func openCodeHasActiveThinkingTag(answer string) bool {
	if !strings.Contains(answer, deepSeekThinkingStart) && !strings.Contains(answer, deepSeekThinkingEnd) {
		return false
	}
	masked := []byte(answer)
	document := openCodeMarkdown.Parse(text.NewReader([]byte(answer)))
	walkOpenCodeLiterals(answer, document, func(segment text.Segment, code bool) bool {
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
func walkOpenCodeLiterals(source string, document ast.Node, visit func(segment text.Segment, code bool) bool) bool {
	visitLines := func(segments *text.Segments, code bool) bool {
		for i := range segments.Len() {
			if visit(segments.At(i), code) {
				return true
			}
		}
		return false
	}
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

func openCodeParagraphStart(document ast.Node, before string) int {
	marker := len(before)
	start := strings.LastIndexByte(before, '\n') + 1
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.Kind() {
		case ast.KindParagraph, ast.KindTextBlock, ast.KindHeading:
			lines := node.Lines()
			for i := range lines.Len() {
				segment := lines.At(i)
				if segment.Start <= marker && marker < segment.Stop {
					start = lines.At(0).Start
					return ast.WalkStop, nil
				}
			}
		}
		return ast.WalkContinue, nil
	})
	return start
}

// Prose quotes can span lines within a paragraph. The caller has already
// masked any parsed Markdown code in that paragraph.
func openCodeInlineQuote(line, suffix string) bool {
	lineSuffix, _, _ := strings.Cut(suffix, "\n")
	hasClosingSingleQuote := strings.ContainsAny(lineSuffix, "'’")
	var quote, closeQuote rune
	var quoteDepth int
	for next := 0; next < len(line); {
		i := next
		c, width := utf8.DecodeRuneInString(line[i:])
		next = i + width
		switch {
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
	return quote != 0 || strings.HasSuffix(line, "\\")
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

func openCodeWordByte(c byte) bool {
	return c >= 0x80 || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
