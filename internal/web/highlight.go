package web

import (
	"bytes"
	"html/template"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

const (
	chromaLightStyle = "github"
	// onedark is tuned for a ~#282c34 slate background, which matches the
	// codebeam-dark base surface; github-dark assumes a near-black background and
	// its token colors wash out once we drop the background and show code on it.
	chromaDarkStyle   = "onedark"
	chromaDarkTheme   = "codebeam-dark"
	maxHighlightLines = 5000
)

// chromaFormatter emits class-based token spans (no surrounding <pre>), so each
// line can be wrapped in our own clickable row markup. Colors come from the
// stylesheet served at /assets/chroma.css, not inline styles.
var chromaFormatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.PreventSurroundingPre(true),
)

// highlightLines tokenises file content and returns one HTML fragment per line.
// It falls back to escaped plain text for very large files or on any error, so
// the file view always renders. Name tokens whose text is a key in symbolLines
// are wrapped in an anchor that jumps to that definition line (#L<n>), giving
// zero-config go-to-definition for any ctags-supported language.
func highlightLines(path string, content []byte, symbolLines map[string]int) []template.HTML {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")

	if strings.Count(text, "\n")+1 > maxHighlightLines {
		return plainLines(text)
	}

	lexer := lexers.Match(path)
	if lexer == nil {
		lexer = lexers.Analyse(text)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}

	iterator, err := lexer.Tokenise(nil, text)
	if err != nil {
		return plainLines(text)
	}

	lines := chroma.SplitTokensIntoLines(iterator.Tokens())
	out := make([]template.HTML, 0, len(lines))
	var buf strings.Builder
	for _, lineTokens := range lines {
		buf.Reset()
		for i, tok := range lineTokens {
			value := tok.Value
			// Drop the trailing newline so <pre> rows don't render a blank line.
			if i == len(lineTokens)-1 {
				value = strings.TrimSuffix(value, "\n")
			}
			renderToken(&buf, tok.Type, value, symbolLines)
		}
		out = append(out, template.HTML(buf.String()))
	}
	return out
}

// renderToken writes one chroma token as HTML. Name tokens that match a known
// definition become a go-to-definition anchor that keeps the token's colour
// class (so syntax highlighting is preserved); other tokens render as the
// class-based span chroma's HTML formatter would emit.
func renderToken(buf *strings.Builder, tt chroma.TokenType, value string, symbolLines map[string]int) {
	if value == "" {
		return
	}
	esc := template.HTMLEscapeString(value)
	class := tokenClass(tt)
	if line, ok := symbolLines[value]; ok && tt.Category() == chroma.Name {
		buf.WriteString(`<a class="codesym`)
		if class != "" {
			buf.WriteByte(' ')
			buf.WriteString(class)
		}
		buf.WriteString(`" href="#L`)
		buf.WriteString(strconv.Itoa(line))
		buf.WriteString(`" title="Go to definition (line `)
		buf.WriteString(strconv.Itoa(line))
		buf.WriteString(`)">`)
		buf.WriteString(esc)
		buf.WriteString(`</a>`)
		return
	}
	if class != "" {
		buf.WriteString(`<span class="`)
		buf.WriteString(class)
		buf.WriteString(`">`)
		buf.WriteString(esc)
		buf.WriteString(`</span>`)
		return
	}
	buf.WriteString(esc)
}

// tokenClass resolves the CSS class chroma's HTML formatter uses for a token
// type, walking up to the sub-category and category like the formatter does.
func tokenClass(tt chroma.TokenType) string {
	if c, ok := chroma.StandardTypes[tt]; ok {
		return c
	}
	if c, ok := chroma.StandardTypes[tt.SubCategory()]; ok {
		return c
	}
	if c, ok := chroma.StandardTypes[tt.Category()]; ok {
		return c
	}
	return ""
}

// plainLines returns HTML-escaped lines, mirroring chroma's per-line output but
// without syntax coloring.
func plainLines(text string) []template.HTML {
	raw := splitTextLines(text)
	out := make([]template.HTML, len(raw))
	for i, line := range raw {
		out[i] = template.HTML(template.HTMLEscapeString(line))
	}
	return out
}

func splitTextLines(text string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// buildChromaCSS generates a single stylesheet with the light theme as the
// default and the dark theme scoped under the dark daisyUI theme attribute.
func buildChromaCSS() string {
	var light, dark bytes.Buffer
	_ = chromaFormatter.WriteCSS(&light, styles.Get(chromaLightStyle))
	_ = chromaFormatter.WriteCSS(&dark, styles.Get(chromaDarkStyle))
	return scopeChromaCSS(light.String(), "") +
		scopeChromaCSS(dark.String(), `[data-theme="`+chromaDarkTheme+`"] `) +
		codeSymbolCSS
}

// codeSymbolCSS styles go-to-definition anchors so they keep their syntax colour
// and only reveal as links on hover. The color rule is intentionally written as
// "a.codesym" (one element + one class) so it is LESS specific than the token
// colour rules ".chroma .nf" (two classes): coloured tokens keep their colour,
// while tokens with no colour rule inherit the surrounding text colour instead of
// the browser's default link blue.
const codeSymbolCSS = `a.codesym{color:inherit;text-decoration:none}
a.codesym:hover{text-decoration:underline;text-decoration-style:dotted;text-underline-offset:2px;cursor:pointer}
`

// scopeChromaCSS keeps only token color rules (".chroma .xx"), dropping the
// background rules so the daisyUI surface shows through, and optionally prefixes
// every selector to scope it to a theme.
func scopeChromaCSS(css, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(css, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, ".bg ") || strings.Contains(line, ".chroma {") {
			continue
		}
		if prefix != "" {
			line = strings.ReplaceAll(line, ".chroma", prefix+".chroma")
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
