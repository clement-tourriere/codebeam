package web

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
)

func TestHighlightLinesLinksDefinedSymbols(t *testing.T) {
	content := []byte("package main\n\nfunc Greet() string { return helper() }\n")
	lines := highlightLines("main.go", content, map[string]int{"Greet": 3})

	var joined strings.Builder
	for _, l := range lines {
		joined.WriteString(string(l))
		joined.WriteByte('\n')
	}
	got := joined.String()

	// The defined symbol is wrapped in a go-to-definition anchor that keeps its
	// syntax-colour class.
	if !strings.Contains(got, `href="#L3"`) || !strings.Contains(got, `>Greet</a>`) {
		t.Fatalf("Greet was not linked to its definition:\n%s", got)
	}
	// An identifier that is not a known definition must not become a link.
	if strings.Contains(got, `>helper</a>`) {
		t.Fatalf("helper should not be linked:\n%s", got)
	}
	// Syntax highlighting is preserved (keyword spans still present).
	if !strings.Contains(got, "<span class=") {
		t.Fatalf("expected chroma spans to remain:\n%s", got)
	}
}

func TestHighlightLinesWithoutSymbolsIsPlainHighlight(t *testing.T) {
	content := []byte("package main\n\nfunc Greet() {}\n")
	lines := highlightLines("main.go", content, nil)
	for _, l := range lines {
		if strings.Contains(string(l), "codesym") {
			t.Fatalf("no anchors expected without symbols: %s", l)
		}
	}
}

func TestTokenClassResolvesCategories(t *testing.T) {
	// A concrete subtype resolves to a class via the standard chroma mapping.
	if got := tokenClass(chroma.KeywordDeclaration); got == "" {
		t.Fatal("expected a non-empty class for a keyword token")
	}
	if got := tokenClass(chroma.NameFunction); got == "" {
		t.Fatal("expected a non-empty class for a function-name token")
	}
}
