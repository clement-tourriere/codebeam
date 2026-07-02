package structural

import (
	"fmt"
	"sort"
	"strings"
)

// Languages bundled into astgrep.wasm. Must stay in sync with ENABLED_LANGS
// in wasm/astgrep/src/lib.rs and the grammar features in its Cargo.toml.
// Keys are the canonical names the shim accepts; values are the file
// extensions scanned for that language (mirroring ast-grep's own mapping).
var langExtensions = map[string][]string{
	"bash":       {".bash", ".bats", ".ksh", ".sh", ".zsh"},
	"c":          {".c", ".h"},
	"go":         {".go"},
	"java":       {".java"},
	"javascript": {".cjs", ".js", ".mjs", ".jsx"},
	"json":       {".json"},
	"python":     {".py", ".py3", ".pyi", ".bzl"},
	"rust":       {".rs"},
	"tsx":        {".tsx"},
	"typescript": {".ts", ".cts", ".mts"},
	"yaml":       {".yaml", ".yml"},
}

var langAliases = map[string]string{
	"golang": "go",
	"js":     "javascript",
	"jsx":    "javascript",
	"kt":     "kotlin",
	"py":     "python",
	"rb":     "ruby",
	"rs":     "rust",
	"sh":     "bash",
	"ts":     "typescript",
	"yml":    "yaml",
}

// CanonicalLang normalizes a user-supplied language name to the canonical
// name understood by the WASM shim. It returns an error naming the supported
// set when the language is unknown or not bundled.
func CanonicalLang(lang string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(lang))
	if alias, ok := langAliases[l]; ok {
		l = alias
	}
	if _, ok := langExtensions[l]; !ok {
		return "", fmt.Errorf("unsupported language %q (supported: %s)", lang, strings.Join(SupportedLanguages(), ", "))
	}
	return l, nil
}

// SupportedLanguages returns the canonical names of all bundled languages.
func SupportedLanguages() []string {
	langs := make([]string, 0, len(langExtensions))
	for l := range langExtensions {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	return langs
}

func extensionSet(lang string) map[string]bool {
	set := make(map[string]bool)
	for _, ext := range langExtensions[lang] {
		set[ext] = true
	}
	return set
}
