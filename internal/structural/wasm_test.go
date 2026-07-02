package structural

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestMatcherMetaListsLanguages(t *testing.T) {
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	langs, err := m.Languages(context.Background())
	if err != nil {
		t.Fatalf("Languages: %v", err)
	}
	want := SupportedLanguages()
	if len(langs) != len(want) {
		t.Fatalf("module languages %v do not match Go-side list %v", langs, want)
	}
	for _, l := range want {
		found := false
		for _, got := range langs {
			if got == l {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("language %q missing from module", l)
		}
	}
}

func TestMatchBytesGoPattern(t *testing.T) {
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	src := []byte(`package main

import "errors"

func run() error {
	err := step()
	if err != nil {
		return err
	}
	return nil
}

func step() error { return errors.New("boom") }
`)
	matches, truncated, err := m.MatchBytes(context.Background(), "if $ERR != nil { $$$BODY }", "go", src, 0)
	if err != nil {
		t.Fatalf("MatchBytes: %v", err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(matches) != 1 {
		t.Fatalf("want 1 match, got %d: %+v", len(matches), matches)
	}
	got := matches[0]
	text := string(src[got.ByteStart:got.ByteEnd])
	if !strings.HasPrefix(text, "if err != nil {") {
		t.Errorf("match text = %q", text)
	}
	if got.StartLine != 6 {
		t.Errorf("StartLine = %d, want 6 (zero-based)", got.StartLine)
	}
	errSpan, ok := got.Vars["ERR"]
	if !ok {
		t.Fatalf("missing $ERR capture; vars = %+v", got.Vars)
	}
	if string(src[errSpan.Start:errSpan.End]) != "err" {
		t.Errorf("$ERR = %q, want \"err\"", src[errSpan.Start:errSpan.End])
	}
	body, ok := got.MultiVars["BODY"]
	if !ok {
		t.Fatalf("missing $$$BODY capture; multiVars = %+v", got.MultiVars)
	}
	if !strings.Contains(string(src[body.Start:body.End]), "return err") {
		t.Errorf("$$$BODY = %q", src[body.Start:body.End])
	}
}

func TestMatchBytesInvalidPattern(t *testing.T) {
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	err := m.Validate(context.Background(), "if err != nil {", "go")
	if err == nil {
		t.Fatal("want error for unparseable pattern")
	}
}

func TestMatchBytesUnsupportedLanguage(t *testing.T) {
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	err := m.Validate(context.Background(), "foo", "cobol")
	if err == nil {
		t.Fatal("want error for unsupported language")
	}
}

func TestMatchBytesMaxMatches(t *testing.T) {
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	src := []byte("console.log(1)\nconsole.log(2)\nconsole.log(3)\n")
	matches, truncated, err := m.MatchBytes(context.Background(), "console.log($$$)", "javascript", src, 2)
	if err != nil {
		t.Fatalf("MatchBytes: %v", err)
	}
	if len(matches) != 2 || !truncated {
		t.Fatalf("want 2 truncated matches, got %d truncated=%v", len(matches), truncated)
	}
}

// BenchmarkMatchBytes measures per-file structural matching over a realistic
// Go source file (this package's engine.go), pattern precompiled and cached.
func BenchmarkMatchBytes(b *testing.B) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		b.Fatal(err)
	}
	m := &Matcher{}
	b.Cleanup(func() { _ = m.Close(context.Background()) })
	// Warm up: compile module + pattern.
	if _, _, err := m.MatchBytes(context.Background(), "if $ERR != nil { $$$ }", "go", src, 0); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		if _, _, err := m.MatchBytes(context.Background(), "if $ERR != nil { $$$ }", "go", src, 0); err != nil {
			b.Fatal(err)
		}
	}
}
