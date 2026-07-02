# Structural Search with ast-grep — Design & Implementation

> **Status: implemented.** Structural search ships in-process from day one — no
> subprocess, no external binary, no cgo. This document records the design as
> built, the measured numbers, and the remaining roadmap. (The original plan
> proposed a subprocess v0 followed by a WASI build of the ast-grep *CLI*;
> both were superseded by the thin-core-shim design below.)

## Summary

Codebeam offers **Structural Search** (AST-shape matching) alongside Zoekt's
lexical/symbol search:

- **Zoekt** remains the engine for fast lexical, regex, normalized, and
  ctags-backed symbol search.
- **ast-grep** powers syntax-tree-aware pattern matching: "find code shaped
  like this" — `if $ERR != nil { $$$ }`, `console.log($$$)`,
  `useEffect($FN, [])`.
- Embedding/vector search remains a separate future feature for true
  natural-language semantic retrieval. Structural search is deliberately
  **not** called "semantic search": ast-grep matches syntax trees; it does no
  type inference, cross-file resolution, or conceptual matching.

## The key architectural decision: wrap `ast-grep-core`, not the CLI

ast-grep is layered: `ast-grep-core` (the pattern/matcher engine) is a plain
Rust library, and the CLI is one shell around it. Instead of porting the CLI
to WASI (dragging in its file walker, `.gitignore` engine, and rayon
parallelism — dead weight in single-threaded WASM), Codebeam ships a
**~250-line Rust shim** (`wasm/astgrep/`) that wraps `ast-grep-core` +
statically compiled tree-sitter grammars and exposes a bytes-in/JSON-out ABI:

```
sg_alloc(len) -> ptr             allocate a buffer the host writes into
sg_dealloc(ptr, len)             release it
sg_compile(pattern, lang) -> id  parse + validate a pattern (0 = error)
sg_match(id, src, max) -> json   match one file's bytes
sg_free_pattern(id)              drop a compiled pattern
sg_last_error() -> str           last error message
sg_meta() -> json                engine version + bundled languages
```

The module is compiled to `wasm32-wasip1`, embedded via `go:embed`
(`internal/structural/astgrep.wasm`, ~5.7 MB after `wasm-opt`), and executed
on **wazero** — which was already in the dependency graph via Zoekt's use of
`wasilibs/go-re2` (the direct precedent for this pattern: RE2 is C++ run as
WASI on wazero behind a Go API).

**Go owns everything except matching**: file enumeration, filtering, caps,
timeouts, parallelism, and result adaptation. The WASM side only ever sees
one buffer of bytes at a time. This bytes-in design is what made three
originally-hard problems nearly free:

1. **Caps are trivial.** Stop feeding files when the match/file budget is
   hit. No stream cancellation, no missing `--max-count` workaround.
2. **Branch search costs nothing extra.** A branch-filtered search reads
   committed blobs straight from the git object store (`git ls-tree` +
   `git cat-file --batch`, exposed as `Indexer.ListBranchFiles` /
   `Indexer.OpenBlobReader`) and feeds those bytes to the matcher. No temp
   trees, no clone mutation, full parity with Zoekt's multi-branch indexing.
3. **A future Zoekt prefilter composes naturally.** Candidate file contents
   from Zoekt can be fed directly to the matcher; the prefilter and the
   engine share a data type (bytes).

### Why the alternatives lost

| Approach | External install? | In-process? | Single static binary? | cgo? | Verdict |
| --- | ---: | ---: | ---: | ---: | --- |
| **`ast-grep-core` WASI shim on wazero** | No | Yes | Yes (one embedded `.wasm`) | No | **Shipped.** |
| WASI build of the ast-grep CLI | No | Yes | Yes | No | Rejected — drags in walker/rayon/gitignore; upstream has no WASI build to reuse (its only wasm target is a wasm-bindgen/emscripten browser playground); caps and branches stay awkward. |
| Require `ast-grep` on PATH (subprocess) | Yes | No | Weakened | No | Rejected as a runtime path — kept only as a **CI test oracle** (see Testing). |
| Managed native sidecar binary | No | No | No (N artifacts) | No | Rejected — multi-artifact releases, still out-of-process. |
| Rust static lib via cgo | No | Yes | Partly | Yes | Rejected — breaks `CGO_ENABLED=0` cross-compilation. |
| Reimplement matching in Go | No | Yes | Yes | No | Rejected — loses upstream pattern semantics/grammar compatibility. |

## What shipped

### Rust shim (`wasm/astgrep/`)

- Pins `ast-grep-core` / `ast-grep-language` **0.44.0** with
  `default-features = false` and per-grammar features. Bundled languages:
  **bash, c, go, java, javascript, json, python, rust, tsx, typescript,
  yaml** (`ENABLED_LANGS` in `src/lib.rs` must stay in sync with
  `Cargo.toml`; adding a language is one feature flag + one enum entry +
  one Go map entry).
- Patterns rejected with real errors: `Pattern::try_new` failures and
  `pattern.has_error()` (the upstream check for patterns that parse with
  ERROR nodes) both surface a message to the user.
- Match output: zero-based byte offsets + line numbers into the exact buffer
  the host passed, plus `$VAR`/`$$$VAR` metavariable spans. The Go adapter
  converts to Codebeam's one-based lines.
- `build.sh` compiles with wasi-sdk (auto-downloaded to `~/.cache/wasi-sdk`),
  `-msimd128 -mbulk-memory -mnontrapping-fptoint`, then `wasm-opt -O3`.
  `mise run wasm:build` wraps it. The `.wasm` artifact is committed so Go
  builds need no Rust toolchain.

### Go engine (`internal/structural/`)

- **`Matcher`** (`wasm.go`): wazero runtime with a persistent compilation
  cache (`<DataDir>/wazero-cache`, so the module compiles once per machine),
  a pool of module instances (default `min(GOMAXPROCS, 8)`; WASM is
  single-threaded, parallelism = instances), per-instance compiled-pattern
  cache, 1 GiB memory cap per instance. Trapped instances are discarded, not
  reused. `WithCloseOnContextDone` is deliberately **off** — it cost 3× on
  tree-sitter's hot loops (31 ms → 10 ms per file); timeouts are enforced
  between files instead.
- **`Engine`** (`engine.go`): repo selection mirroring Zoekt's semantics
  (`Allowed` is the permission gate; filters naming repos outside it are
  errors), worktree walking with the indexer's skip-dir rules, extension
  filtering per language, regex path filter, binary/non-UTF-8 skipping,
  2 MiB per-file cap (same as the indexer), parallel produce/match, global
  file (5000) and match (1000) budgets plus a 15 s timeout — all
  env-configurable (`CODEBEAM_STRUCTURAL_TIMEOUT` / `_MAX_FILES` /
  `_MAX_MATCHES`). A cap/timeout mid-scan degrades to partial results with
  `Truncated` set, never an error.
- Results adapt into `codesearch.Result`/`FileMatch`/`LineMatch` so the
  existing web/API/MCP rendering is reused unchanged. `LineMatch` gained
  `MetaVars map[string]string`; `Result` gained `Truncated`.

### Branch handling (shipped, not deferred)

- No branch filter → the **live worktree**: local repos search uncommitted
  state (Codebeam's superpower), remote clones search the checked-out
  primary branch.
- `branch=<name>` → committed blobs from the git object store, resolving
  `refs/heads/<name>` then `refs/remotes/origin/<name>` (remote clones only
  materialize the primary branch as a worktree; other indexed branches live
  under `refs/remotes/origin/*`). Unknown branches are a clear error, not a
  silent empty result.

### Surfaces

- **Web** (`templates/search.html`): a "Structural (AST)" toggle, a
  language select (required), pattern examples, an engine badge, a
  `truncated` badge, and metavariable captures rendered under each match.
- **API**: `GET /api/search?mode=structural&lang=go&q=<pattern>` plus the
  existing `repo`/`branch`/`path` filters. The response now carries
  `engine` (`"zoekt"` or `"structural"`), `engine_query`, `truncated`, and
  per-line `meta_vars`. `zoekt_query` is preserved for compatibility.
- **MCP**: a `structural_search` tool (`pattern`, `lang` required; `repo`,
  `branch`, `path`, `max_results` optional) alongside `search_code` /
  `symbol_search` / `find_references`. Metavariable captures are printed
  with each match so agents can see what `$VAR` bound to.
- **CLI (`codebeam ctx`)**: not wired yet (deliberately — MCP/API are where
  agents live).

## Performance (measured, Apple M3 Pro)

- Per-file matching: **~7 ms** for a 14 KB Go file (~2 MB/s per instance);
  the pool scales this by up to 8×.
- End-to-end `if $ERR != nil { $$$ }` over the Codebeam repo (360 matches in
  31 files): **~350 ms cold** (instance spin-up) / **~50 ms warm**.
- Native ast-grep is roughly 5–7× faster per file; the wazero overhead is
  the price of zero-install in-process execution and is bounded by the
  caps/timeout above. Structural search is framed in the product as a
  separate, slower tool than the sub-100 ms lexical path — right for agents
  and precise sweeps, not the as-you-type box.

Startup: the 5.7 MB module AOT-compiles once and is cached on disk; warm
boots reuse the cache.

## Testing

- Unit tests for the matcher ABI (captures, truncation, error paths,
  unsupported languages) and the engine (worktree scan, skip dirs, path
  filters, caps, repo permission errors, branch-blob search including the
  branch-not-in-worktree case).
- **Differential tests against the real ast-grep CLI** as a semantics
  oracle (`differential_test.go`): the same patterns over Go / JS / Python /
  TSX / Rust corpora must produce byte-identical match ranges. Skipped when
  `ast-grep` is not installed; the pinned engine version (0.44.0) matches
  Homebrew's current CLI. This replaces the old plan's "subprocess v0" —
  the CLI's value (validating semantics) without shipping it.
- Web API tests (`mode=structural` happy path + lang-required error), a
  full-page template render test, and MCP session tests (tool listed,
  matches with `$NAME = main` captures, lang-required error).

## Remaining roadmap

1. **Zoekt candidate prefilter** — extract conservative literals from the
   pattern (`client.Do($REQ)` → `client`, `Do`), query Zoekt first, feed
   candidate contents to the matcher. Must never introduce false negatives:
   patterns with no safe literals fall back to a direct scan.
2. **YAML rules** — `ast-grep-config` is also a crate; the shim can grow a
   `compile_rule` export for relational rules and the `context`/`selector`
   escape hatch for ambiguous patterns. POST endpoint (query strings are
   too fragile for YAML).
3. **More languages** — c++/c#/ruby/php/kotlin/swift/scala are one feature
   flag away each; weigh module size (~0.3–1.5 MB per grammar).
4. **`codebeam ctx --mode structural`** for packed-context CLI users.
5. **True semantic search** (embeddings + reranking) as its own engine,
   unrelated to this one.

## Risks & mitigations (as-built)

| Risk | Mitigation |
| --- | --- |
| Matching semantics drift from upstream ast-grep | Version pinned (0.44.0); differential CI oracle against the real CLI. |
| Grammar/tree-sitter version skew | The shim pins core + grammars as one set; upgrades bump everything together and rerun the oracle. |
| Runaway parse can't be preempted mid-file (CloseOnContextDone off) | Parses are single-digit ms and tree-sitter is effectively linear; timeouts bound between files; a trapped instance is discarded. |
| Large scans are slow | File/match/time caps with a visible `truncated` flag; ext/path/repo filters; prefilter is the planned structural fix. |
| Pattern learning curve ("why didn't this match") | Real parse errors (including the ERROR-node check), UI examples, metavariable display; `context`/`selector` arrives with YAML rules. |
| Users confuse structural with semantic search | Product naming is "Structural (AST)"; docs state what it does not do. |
| Module size in the binary (+5.7 MB) | Only exposed languages are bundled; `wasm-opt -O3`; acceptable against the zero-install win. |
