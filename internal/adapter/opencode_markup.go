package adapter

import (
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
	line := before[strings.LastIndexByte(before, '\n')+1:]
	if openCodeInlineQuote(line, after) {
		return true
	}
	containsMarker := func(segment text.Segment) bool {
		return segment.Start <= marker && marker+len(deepSeekThinkingEnd) <= segment.Stop
	}
	containsMarkerInLines := func(segments *text.Segments) bool {
		for i := range segments.Len() {
			if containsMarker(segments.At(i)) {
				return true
			}
		}
		return false
	}

	document := openCodeMarkdown.Parse(text.NewReader([]byte(reasoning)))
	protected := false
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.CodeBlock:
			protected = containsMarkerInLines(n.Lines())
		case *ast.FencedCodeBlock:
			protected = containsMarkerInLines(n.Lines()) || n.Info != nil && containsMarker(n.Info.Segment)
		case *ast.Text:
			protected = n.Parent().Kind() == ast.KindCodeSpan && containsMarker(n.Segment)
		case *ast.HTMLBlock:
			// CommonMark's verbatim HTML block types cover comments,
			// processing instructions, declarations, CDATA and preformatted
			// elements. A standalone ordinary closing tag can be the marker.
			if n.HTMLBlockType <= ast.HTMLBlockType5 {
				protected = containsMarkerInLines(n.Lines()) || containsMarker(n.ClosureLine)
			}
		case *ast.RawHTML:
			// A marker in an attribute or comment is part of a larger
			// token. An ordinary closing tag is a token of its own.
			for i := range n.Segments.Len() {
				segment := n.Segments.At(i)
				if containsMarker(segment) && segment.Len() != len(deepSeekThinkingEnd) {
					protected = true
					break
				}
			}
		}
		if !protected && node.Type() == ast.TypeBlock && containsMarkerInLines(node.Lines()) {
			for parent := node.Parent(); parent != nil; parent = parent.Parent() {
				if parent.Kind() == ast.KindBlockquote {
					protected = true
					break
				}
			}
		}
		if protected {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return protected
}

// Inline prose quoting is local to the marker line. An unbalanced apostrophe
// or backtick in an earlier line cannot invalidate a later completed turn.
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
			case c == closeQuote:
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
