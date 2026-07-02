package main

import (
	"bytes"
	"strings"
	"testing"

	codesearch "github.com/ctourriere/codebeam/internal/search"
)

func TestWriteContextBundleIncludesCitationsAndSnippets(t *testing.T) {
	result := codesearch.Result{
		Query:      "UniqueNeedle",
		FileCount:  1,
		MatchCount: 1,
		Files: []codesearch.FileMatch{{
			Repository: "local/repo",
			Path:       "main.go",
			Lines: []codesearch.LineMatch{{
				Number:   2,
				Before:   []codesearch.ContextLine{{Number: 1, Text: "package main"}},
				Segments: []codesearch.Segment{{Text: "// "}, {Text: "UniqueNeedle", Match: true}},
				After:    []codesearch.ContextLine{{Number: 3, Text: "func main() {}"}},
			}},
		}},
	}

	var out bytes.Buffer
	writeContextBundle(&out, result, 2000, 10)
	got := out.String()
	for _, want := range []string{"# Codebeam context", "Query: `UniqueNeedle`", "## local/repo:main.go", "1: package main", "2: // UniqueNeedle", "3: func main() {}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("context bundle missing %q:\n%s", want, got)
		}
	}
}

func TestWriteContextBundleHonorsMaxFiles(t *testing.T) {
	result := codesearch.Result{
		Query:      "needle",
		FileCount:  2,
		MatchCount: 2,
		Files: []codesearch.FileMatch{
			{Repository: "local/one", Path: "one.go"},
			{Repository: "local/two", Path: "two.go"},
		},
	}

	var out bytes.Buffer
	writeContextBundle(&out, result, 2000, 1)
	got := out.String()
	if !strings.Contains(got, "## local/one:one.go") {
		t.Fatalf("first file missing:\n%s", got)
	}
	if strings.Contains(got, "## local/two:two.go") {
		t.Fatalf("second file should be omitted:\n%s", got)
	}
	if !strings.Contains(got, "Omitted 1 additional files") {
		t.Fatalf("omission notice missing:\n%s", got)
	}
}

func TestPlainSegments(t *testing.T) {
	got := plainSegments([]codesearch.Segment{{Text: "a"}, {Text: "b", Match: true}})
	if got != "ab" {
		t.Fatalf("got %q", got)
	}
}
