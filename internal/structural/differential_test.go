package structural

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// TestDifferentialAgainstAstGrepCLI cross-checks the embedded WASM matcher
// against the real ast-grep CLI (the semantics oracle). It is skipped when
// the CLI is not installed; install ast-grep to run it (the pinned engine
// version is 0.44.0 — see wasm/astgrep/Cargo.toml).
func TestDifferentialAgainstAstGrepCLI(t *testing.T) {
	bin, err := exec.LookPath("ast-grep")
	if err != nil {
		t.Skip("ast-grep CLI not installed; differential test skipped")
	}

	cases := []struct {
		name    string
		lang    string
		file    string
		pattern string
		source  string
	}{
		{
			name: "go if err", lang: "go", file: "main.go",
			pattern: "if $ERR != nil { $$$ }",
			source: `package main

func run() error {
	err := step()
	if err != nil {
		return err
	}
	if e2 := other(); e2 != nil {
		return e2
	}
	return nil
}
`,
		},
		{
			name: "js console.log", lang: "javascript", file: "app.js",
			pattern: "console.log($$$)",
			source: `function f(x) {
  console.log(1);
  console.log("two", x);
  console.warn(3);
  const log = () => console.log();
}
`,
		},
		{
			name: "python method call", lang: "python", file: "app.py",
			pattern: "$OBJ.get($KEY, $DEFAULT)",
			source: `def f(d):
    a = d.get("a", None)
    b = d.get("b")
    c = d.get("c", 1)
    return a, b, c
`,
		},
		{
			name: "tsx useEffect empty deps", lang: "tsx", file: "app.tsx",
			pattern: "useEffect($FN, [])",
			source: `import { useEffect } from "react";
export function C() {
  useEffect(() => { console.log("mount"); }, []);
  useEffect(() => { console.log("dep"); }, [x]);
  return null;
}
`,
		},
		{
			name: "rust unwrap", lang: "rust", file: "lib.rs",
			pattern: "$E.unwrap()",
			source: `fn f() {
    let a = maybe().unwrap();
    let b = maybe().unwrap_or(1);
    let c = other().unwrap();
}
`,
		},
	}

	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.source), 0o644); err != nil {
				t.Fatal(err)
			}

			cliRanges := cliMatchRanges(t, bin, tc.pattern, tc.lang, path)
			wasmMatches, _, err := m.MatchBytes(context.Background(), tc.pattern, tc.lang, []byte(tc.source), 0)
			if err != nil {
				t.Fatalf("MatchBytes: %v", err)
			}
			wasmRanges := make([]string, 0, len(wasmMatches))
			for _, wm := range wasmMatches {
				wasmRanges = append(wasmRanges, fmt.Sprintf("%d-%d", wm.ByteStart, wm.ByteEnd))
			}
			sort.Strings(wasmRanges)

			if len(cliRanges) == 0 {
				t.Fatalf("oracle found no matches; broken test case?")
			}
			if fmt.Sprint(cliRanges) != fmt.Sprint(wasmRanges) {
				t.Errorf("divergence from ast-grep CLI:\n  cli:  %v\n  wasm: %v", cliRanges, wasmRanges)
			}
		})
	}
}

func cliMatchRanges(t *testing.T, bin, pattern, lang, path string) []string {
	t.Helper()
	out, err := exec.Command(bin, "run", "--pattern", pattern, "--lang", lang, "--json=compact", path).Output()
	if err != nil {
		t.Fatalf("ast-grep CLI: %v", err)
	}
	var matches []struct {
		Range struct {
			ByteOffset struct {
				Start int `json:"start"`
				End   int `json:"end"`
			} `json:"byteOffset"`
		} `json:"range"`
	}
	if err := json.Unmarshal(out, &matches); err != nil {
		t.Fatalf("decode CLI output: %v\n%s", err, out)
	}
	ranges := make([]string, 0, len(matches))
	for _, m := range matches {
		ranges = append(ranges, fmt.Sprintf("%d-%d", m.Range.ByteOffset.Start, m.Range.ByteOffset.End))
	}
	sort.Strings(ranges)
	return ranges
}
