# Codebeam — Product Vision & Strategy

*The agent-native, live, local-first (and org-server) code search tool.*

> **Thesis:** Codebeam searches the code you're writing *right now*, across every repo you own — and hands AI agents instant, structured, token-cheap recall that `grep` and the cloud incumbents can't. It ships as **one engine in two modes**: a zero-config solo binary and a hosted team/org server, bridged by federation.

This document captures (1) where codebeam is today, (2) the competitive landscape, and (3) the full north-star feature vision. It's intentionally maximalist — the complete menu of ideas, organized by pillar and tagged for differentiation — not a prioritized build plan. Sequencing is a separate exercise.

---

# Part 1 — Where Codebeam Is Today

A clean, well-architected local-first code search tool. Findings from a full codebase exploration:

**Stack.** Go (net/http, `html/template`); **Zoekt** (trigram index) as the search engine; **SQLite** (`modernc.org/sqlite`) for metadata; **Chroma** for syntax highlighting; `go-ctags` present as a dependency; frontend is **TailwindCSS 4 + DaisyUI 5 + HTMX 2**; ships as a single binary; `mise` for tasks; `hk` (`hk.pkl`) for dev git hooks (gofmt/vet/build/test).

**Modules** (`internal/`): `config` (env-var config), `store` (SQLite), `search` (Zoekt wrapper), `indexer` (git clone/fetch + Zoekt build), `watcher` (fsnotify live-refresh for local repos), `web` (handlers, templating, auth, highlighting, plus a JSON `/api/search` + `/api/read` for agents), `mcp` (stdio Model Context Protocol server: `search_code` / `read_file` / `list_repos`), `codehost` (GitHub/GitLab OAuth + API). The `cmd/codebeam` binary also ships a `ctx` packed-context CLI and an `mcp` subcommand.

**Data model** (SQLite): `users`, `identities` (per-provider OAuth tokens), `repos` (host/clone metadata, default branch, indexed_at), `repo_permissions` (per-user access), `index_jobs` (queued→running→succeeded/failed/cancelled).

**Search path.** `BuildZoektQuery` composes a Zoekt query from user input + `repo`/`path`/`lang` filters → Zoekt searches `.zoekt` shards in `.codebeam/index/` → results (100 files / 1000 matches / 8s cap, 2 context lines) are Chroma-highlighted and linked to a file viewer. Regex + literal supported via Zoekt's native syntax.

**Indexing.** Manual / on-demand per repo. Remote repos: `git clone --depth=1` then `fetch`; local repos: read in place. `filepath.WalkDir` skips `.git`, `node_modules`, `target`, `dist`, `build`, `.venv`, etc. and files > 2MB. One Zoekt shard per repo. Jobs run in background goroutines (cancellable, 20-min timeout). Remote repos can index comma-separated branches in one Zoekt shard; local repos index the checked-out worktree. **Full re-index every time (no incremental).**

**Repos & UI.** GitHub/GitLab OAuth sync, self-managed GitLab via PAT, and local-path repos; per-user permissions. UI: search page (HTMX results), repos grid + manage (tabs/filters/bulk actions/live job polling), repo detail (lazy file tree + code viewer), sources (OAuth/identities), login (dev + OAuth), theme toggle.

**Shipped since this snapshot:** local file-watching → debounced live reindex (`watcher`); a `dirty` freshness badge on working-tree matches; a JSON search/read API for agents; a `codebeam ctx` packed-context CLI; **ctags-backed symbol search** (`sym:` atom, surfaced as a web "Symbols only" toggle, a `sym=1` API flag, and a `symbol_search` MCP tool, with zero-config Universal Ctags auto-detection + `CODEBEAM_CTAGS_PATH`); and a stdio **MCP server** (`codebeam mcp`) exposing a six-tool retrieval toolbox: `search_code`, `symbol_search` (go-to-definition), `find_references` (word-boundary usages), `read_file`, `file_tree`, and `list_repos`.

Code navigation is live in both surfaces: agents get `symbol_search` (go-to-definition) + `find_references` over MCP, and the web file viewer shows a live-ctags **symbol outline** (jump-to-definition + repo-scoped find-references links).

**Gaps today (the roadmap surface):** no remote webhooks or git-hook auto-update; no incremental/delta indexing (still a full Zoekt rebuild); branch-aware browsing is still basic compared with full code-host navigation; web nav is outline-based, not yet clickable-identifier go-to-definition in the code body; no cross-repo symbol/dependency graph; no structural/AST search; no semantic/AI layer; no streamable-HTTP MCP transport or REST/GraphQL API; no saved searches / history / alerts; no keyboard shortcuts; no orgs / teams / SSO / RBAC; no git history / diff / blame search.

---

# Part 2 — The Competitive Landscape

**Sourcegraph (incumbent).** Code search across branches/hosts, Deep Search (NL → AI answers), precise code intelligence (SCIP/LSIF, per-repo indexers), Batch Changes (cross-repo migrations), Code Insights (dashboards), query-assist (`#` NL→query), code monitoring (`type:commit`/`type:diff` triggers) + saved searches, Cody assistant. Heavyweight deploy (k8s/Docker). Amp (agentic coding) spun out.

**Sourcebot (closest twin — also Zoekt + self-hosted Docker).** Code search (regex/boolean/repo/file/branch), file explorer + viewer, **"Ask"** (NL Q&A with inline citations, agentic, BYO LLM key), **MCP server** (search, read files, resolve refs/defs), Code Navigation (Enterprise), broad code hosts (GitHub, GitLab, Bitbucket, Azure DevOps, Gerrit, Gitea), many LLM providers, analytics, permission syncing, SSO, audit logs. Single Docker image, scales to thousands of repos.

**Emerging AI / MCP code-search tools.** Claude Context (hybrid BM25 + dense vector); CodeGrok (AST/tree-sitter + embeddings, "10× context savings" for agents); Serena (symbol-level LSP retrieval, "IDE for your agent"); Codemogger (single-file SQLite, vector + FTS, no Docker/keys); DeepWiki (auto-generated wiki + Mermaid + RAG Q&A + MCP, 50k+ public repos).

**2026 trends.** Hybrid lexical + semantic + rerank (RRF + cross-encoder) wins. "The grep replacement is *three* tools, not one" — lexical, structural, graph — as distinct agent tools. Agents waste huge context on `grep` false positives, and **can't grep repos they don't have cloned**. Local code-embedding models are maturing (privacy + no API key).

**The white space neither incumbent occupies:** searching the **live/uncommitted working tree**; **local auto-update** via file-watch + git hooks; **zero-config code nav** (no SCIP/LSIF); **structural/AST search** in the OSS tier; **local private embeddings**; **shift-left search-lint** (pre-commit); **always-fresh private codebase wiki**; **federation** of personal + team instances; and a **single binary that scales from laptop to org cluster**.

---

# Part 3 — The North-Star Vision

## The One-Line Thesis

> **Codebeam searches the code you're writing *right now*, across every repo you own — and hands AI agents instant, structured, token-cheap recall that `grep` and the cloud incumbents can't.**

Three pillars carry it, with four supporting tracks:

- **Pillar I — Live & Fresh** — search your real working state, always up to date, zero manual reindex.
- **Pillar II — Agent-Native** — the retrieval backend for AI coding agents.
- **Pillar III — Understanding > Matching** — code intelligence + structural + semantic + knowledge, zero-config.
- *Supporting:* **Git-Aware & Time-Travel**, **Search Craft & Human UX**, **Proactive & Collaborative**, **Reach & Ecosystem**.

---

## Two Modes, One Engine

Codebeam is **one product, one engine, two deployment modes** — run either, or both at once and federate them. The index, search, code-intelligence, structural/semantic layers, and the agent MCP toolbox are *identical* across modes; only the topology and access model differ.

**🧑‍💻 Solo mode — `codebeam` (run it like `ripgrep`).** A single binary on your laptop. Auto-discovers local git repos, watches the working tree, searches your *live, uncommitted* state, and exposes a local MCP server to your editor's agent. Zero config, fully private, nothing leaves the machine. This is where the live-working-tree and shift-left superpowers live.

**🏢 Team/org server mode — `codebeam serve` (the shared brain).** A hosted deployment the whole organization points a browser (or an agent) at to see and search *all* of the org's code. Connects to GitHub, GitLab, Bitbucket, Gitea, Azure DevOps, Gerrit — clones and indexes every repo, keeps them fresh via webhooks + scheduled pulls, and serves multi-user search with **SSO, RBAC, and code-host permission sync** so people only see what they're allowed to. Same single binary, started with `serve`; scales from a small box for one team to a multi-tenant cluster for an enterprise. This is codebeam-as-Sourcegraph/Sourcebot — but with the agent-native retrieval and zero-config understanding pillars built in, and a far simpler deploy.

**🔗 Federation bridges them.** A developer's solo instance can federate with the team server, so one search spans *both* "my live working tree" *and* "the whole org's committed code." Your agent gets org-wide recall plus your uncommitted edits in a single query. No incumbent offers this.

| | Solo mode (`codebeam`) | Server mode (`codebeam serve`) |
|---|---|---|
| Runs on | Your laptop | Shared host / cluster |
| Indexes | Local repos + **live working tree** | All org repos from code hosts (committed) |
| Freshness | fsnotify + git hooks (sub-second) | Webhooks + scheduled pulls |
| Users | You | Whole org, multi-user |
| Access control | n/a (it's yours) | SSO/OIDC/SAML, RBAC, code-host permission sync |
| Privacy | Nothing leaves the machine | Stays inside your infra / VPC |
| Agent MCP | Local working-tree context | Org-wide context for every dev & CI bot |

Every pillar below applies to **both** modes unless noted: "live working-tree" features are solo-mode strengths, "org-wide / multi-user" features are server-mode strengths, and the engine underneath is shared.

---

## Legend

- 🟢 **`NEW-TO-CATEGORY`** — not in Sourcebot *or* Sourcegraph; genuine white space.
- 🔵 **`BEYOND-RIVALS`** — exists somewhere, but codebeam does it differently/better (usually: local, private, zero-config, or live).
- ⚙️ **`ENGINE-FREEBIE`** — Zoekt or an existing dependency already supports this; latent capability codebeam just hasn't surfaced.
- ⚪ **`PARITY`** — table-stakes catch-up the incumbents already have; needed to be taken seriously.

---

## Pillar I — Live & Fresh

*Search reflects reality with zero manual effort, in both modes. **Solo mode** is the standout — searching the live, uncommitted working tree is something no cloud tool can copy. **Server mode** keeps the whole org continuously fresh via webhooks + scheduled pulls.*

- 🟢 **`NEW-TO-CATEGORY` — Live working-tree search (dirty/staged/unstaged).** Codebeam indexes and searches the *actual files on disk*, not just the last commit. Edit a function, don't commit, and search finds it. *Why it wins:* the single sharpest differentiator — Sourcegraph/Sourcebot fundamentally can't, because they index server-side commits. *(Fit: an "overlay" Zoekt shard built from the working tree, layered over the committed shard; or a fast in-memory trigram pass over dirty files merged into results.)*

- 🟢 **`NEW-TO-CATEGORY` — Filesystem watcher → sub-second incremental reindex.** `fsnotify` watches each local repo; on save, only changed files are re-indexed (debounced). Fresh within ~1s of saving, no "reindex" button. *(Fit: `fsnotify` is already an indirect dep via Zoekt; add a watcher service + per-file delta indexing.)*

- 🟢 **`NEW-TO-CATEGORY` — Git-hook auto-update for local repos.** `codebeam hook install` drops `post-commit`, `post-merge`, `post-checkout`, `post-rewrite` hooks (composable with the existing `hk` setup) that ping codebeam to reindex the affected repo. *Why it wins:* the "hooks to auto-update" idea — event-driven and instant on your machine vs. server-side polling.

- ⚪ **`PARITY` — Push webhooks for remote repos.** GitHub/GitLab/Gitea push webhooks → enqueue reindex. Table stakes for server mode; `EnqueueReindex(repoID, userID)` is already decoupled from HTTP, so this is a thin receiver + HMAC verification + debounce.

- 🔵 **`BEYOND-RIVALS` — Index-follows-your-checkout.** When you `git switch` a branch, search follows your current branch automatically — no config, no "select branch" dance. Matches a developer's mental model, not an admin's.

- ⚙️ **`ENGINE-FREEBIE` — Multi-branch indexing + branch-scoped search.** Zoekt natively indexes many branches in one shard via a bitmask (identical files stored once). Codebeam indexes only the default branch today. Surface `branch:` filters. "Search `main` and `release/*`" with near-zero extra storage — already paid for in the engine.

- 🔵 **`BEYOND-RIVALS` — Incremental / delta indexing.** Rebuild only what changed (git diff or fsnotify deltas) instead of full re-index. Makes "always fresh" cheap at scale.

- 🟢 **`NEW-TO-CATEGORY` — Worktree & stash awareness.** Search across `git worktree`s and even stashes ("where did I put that WIP?"). Nobody indexes a developer's *ephemeral* state.

- 🔵 **`BEYOND-RIVALS` — Freshness as a first-class signal.** Every result shows how fresh it is: indexed Xs ago, commit hash, and a **`dirty`** badge if the match is from uncommitted changes. Trust — you always know whether you're searching reality or a stale snapshot.

- ⚪ **`PARITY` — Scheduled background refresh.** A lightweight scheduler re-pulls + reindexes remote repos on an interval as a fallback to webhooks. *(Codebeam has no background worker today.)*

---

## Pillar II — Agent-Native

*Codebeam becomes the retrieval layer AI coding agents reach for instead of `grep`. Local, private, instant, multi-repo.*

- 🔵 **`BEYOND-RIVALS` — A retrieval *toolbox* over MCP, not a single "search."** Agents need distinct tools and pick per question. Codebeam's MCP server exposes the full set: `lexical_search` (Zoekt regex/literal), `structural_search` (tree-sitter/AST), `symbol_search`, `find_references` / `goto_definition`, `semantic_search` (embeddings), `read_file` / `read_range` (token-bounded), `repo_map` / `file_tree` / `list_repos`, and `history_search` / `blame` / `diff_search`. *Why it wins:* Sourcebot's MCP does search/read/refs; codebeam ships the *whole* "three-tools-not-one" stack as one local server.

- 🟢 **`NEW-TO-CATEGORY` — Whole-org context for an agent with one repo cloned.** The deepest agent pain point: "you can't `grep` a repo you don't have locally." Codebeam indexes your *entire org* and exposes it over MCP, so Claude Code working in `service-a` instantly finds usages in `service-b` it never cloned. *(Server mode indexes the whole org; a solo instance can federate with the team server for the same reach — and a CI agent can hit the server directly.)* *Why it wins:* turns a single-repo agent into an org-aware one, privately, on your infra.

- 🟢 **`NEW-TO-CATEGORY` — Token-budgeted retrieval ("just enough context").** Tools accept a token budget and return *ranked snippets with minimal surrounding context* — not whole files. Directly attacks the "agents waste thousands of tokens on grep false-positives" problem; the "10× context savings" pitch, but local and exact-grounded.

- 🟢 **`NEW-TO-CATEGORY` — `codebeam ctx` — packed-context CLI.** `codebeam ctx "how is auth middleware wired?"` returns a ready-to-paste, citation-tagged context bundle (ranked snippets across repos, with `file:line@commit`). Pipe it into *any* LLM, harness, or script. Meets agents and scripts where they live (the shell), not just MCP-aware IDEs.

- 🔵 **`BEYOND-RIVALS` — Agentic "Ask Codebeam" (BYO key, local-first).** NL question → plan → multi-tool search loop → cited answer, rendered in the web UI with inline `file:line` citations that open the live file. Parity with Sourcebot "Ask"/Sourcegraph "Deep Search," but grounded in your *live* index and never sending code to a vendor unless you choose a remote model.

- 🔵 **`BEYOND-RIVALS` — Deterministic, local, sub-100ms, private.** No cloud round-trip, reproducible results, code never leaves the machine/infra by construction.

- 🟢 **`NEW-TO-CATEGORY` — Provenance-first results.** Every snippet carries `repo@commit:path:line` plus a freshness/dirty flag, so the agent (and the human reviewing it) can verify against live source. The antidote to hallucinated file paths.

- 🟢 **`NEW-TO-CATEGORY` — Search recipes / saved tool-chains for agents.** Named, parameterized query pipelines (e.g. `find_callers_then_tests($symbol)`) agents invoke as a single tool. Encodes your team's navigation know-how as reusable agent primitives.

- 🔵 **`BEYOND-RIVALS` — Relevance feedback loop (Cursor-style).** Log which results humans and agents actually open/use; tune ranking over time. The index gets smarter about *your* codebase.

- ⚪ **`PARITY` — Multiple transports + harness integration.** stdio + streamable-HTTP MCP, drop-in configs for Claude Code / Cursor / Copilot / Windsurf, and a REST/GraphQL API for everything else.

---

## Pillar III — Understanding > Matching

*Move from "find the string" to "understand the code." Zero-config, every language, private.*

- 🔵 **`BEYOND-RIVALS` — Zero-config code navigation for 100+ languages.** Go-to-definition, find-references, hover docs via **tree-sitter + ctags** (go-ctags is already a dependency) — no language servers, no SCIP upload, no CI step. *Why it wins:* Sourcegraph's *precise* nav requires per-repo SCIP/LSIF; Sourcebot gates nav behind Enterprise. Codebeam gives "good enough" nav for *every* language the moment a repo is indexed.

- 🔵 **`BEYOND-RIVALS` — Optional SCIP precision when available.** If CI uploads SCIP, use it for exact cross-repo nav; otherwise fall back to fuzzy tree-sitter/ctags. Zero-config by default, precise when you invest.

- 🟢 **`NEW-TO-CATEGORY` (in the OSS tier) — Structural / AST search.** Search by code *shape*, not text, via tree-sitter patterns (ast-grep-style): `if err != nil { $$$ }` with no `return`; functions with `>5` params; `useEffect` with empty deps; empty `catch` blocks. *Why it wins:* Sourcegraph's structural search (Comby) is effectively deprecated/limited; Sourcebot lacks it. Same engine later powers codemods. *(Fit: reuse the upstream ast-grep engine rather than reimplement matching — ship subprocess-first as a v0, then move it **in-process by compiling a pinned ast-grep to a WASI WASM module and running it on the `wazero` runtime Codebeam already depends on**. Pure Go, no cgo, one platform-neutral `.wasm` embedded in the binary — the single-static-binary property is preserved. Layer Zoekt candidate-file filtering on top for speed. See `docs/ast-grep-structural-search-plan.md`.)*

- 🔵 **`BEYOND-RIVALS` — Hybrid semantic search, fully local.** BM25/Zoekt + dense embeddings (a local code model like `jina-embeddings-v2-base-code`), fused with RRF and an optional cross-encoder rerank. "Find code that *does* X" even when the words don't match — but embeddings are computed and stored **locally**: no vendor, no API key, no egress.

- 🔵 **`BEYOND-RIVALS` — "Find similar code" / duplication radar.** Embedding nearest-neighbors surface near-duplicates and copy-paste drift across all repos. A refactoring superpower that falls out of the local embedding index for free.

- 🟢 **`NEW-TO-CATEGORY` — Always-fresh private codebase wiki ("DeepWiki for your code, local & live").** Auto-generate per-repo architecture overviews, module maps, and **Mermaid diagrams** from the index + an LLM — kept fresh because they're tied to the live index. DeepWiki proved the category but is cloud + public-repo-oriented; codebeam does it for **private** code, **locally**, **always current**.

- 🟢 **`NEW-TO-CATEGORY` — Org-wide symbol & dependency graph.** Stitch cross-repo references into a graph: "what depends on this module," "blast radius of changing this signature," and **dead-code detection** ("zero references org-wide"). Neither rival surfaces an org-wide *impact* view from search — invaluable for migrations and cleanup.

- 🔵 **`BEYOND-RIVALS` — Symbol cards & inline "explain."** Hover a symbol → definition, doc comment, usage count, last change, owner. "Explain this match / file / function" inline (BYO key).

---

## Pillar IV — Git-Aware & Time-Travel *(supporting)*

*Code has a time dimension. Make all of history searchable.*

- 🔵 **`BEYOND-RIVALS` — Search across full git history (indexed pickaxe at scale).** `git log -S/-G` semantics, indexed and instant across every repo: "when/where was this introduced or removed?"
- ⚙️ **`ENGINE-FREEBIE` — Diff / commit / PR search.** Search added vs. removed lines, commit messages, PR metadata. *(Zoekt + git plumbing; aligns with multi-branch shards.)*
- 🟢 **`NEW-TO-CATEGORY` — Blame-integrated results.** Each matched line can show who wrote it, when, and in which PR — inline in the results list. Answers "who do I ask about this?" at the moment of discovery.
- 🔵 **`BEYOND-RIVALS` — Time-travel view.** "Show me this file / search this repo *as of* commit X or date Y." Code archaeology: watch a function evolve.
- ⚪ **`PARITY` — Author / ownership facets.** Filter and rank by author, team, CODEOWNERS.

---

## Pillar V — Search Craft & Human UX *(supporting)*

*The incumbents are functional but joyless. Win developers' hearts with speed and feel.*

- 🔵 **`BEYOND-RIVALS` — Instant search-as-you-type.** Sub-50ms HTMX live results (Zoekt is fast enough; codebeam currently requires form submit). The difference between "a tool I open" and "a reflex."
- 🟢 **`NEW-TO-CATEGORY` (in this class) — Keyboard-first everything.** `cmd-K` palette, `j/k` result nav, peek-definition without leaving the list, quick-open files, vim bindings. Cheap to build on the existing HTMX UI, and lovable.
- 🔵 **`BEYOND-RIVALS` — Hybrid ranking that feels right.** Boost symbol definitions over usages, recently-changed and frequently-edited files, `src/` over `vendor/`/`test/`, proximity to the active repo. Compounds with the feedback loop (Pillar II).
- 🔵 **`BEYOND-RIVALS` — One query language across all modes.** Compose lexical + structural + symbol + semantic in a single query (e.g. `lang:go sym:Handler ~"rate limiting"`). No tool unifies these in one box.
- ⚪ **`PARITY` — NL → query assist.** Press `#` (Sourcegraph-style) to translate English into a precise codebeam query (BYO key or small local model).
- ⚪ **`PARITY` — Saved searches, search history, scopes/contexts.** Persist queries, recall recent ones, define named repo subsets ("backend," "my team").
- 🔵 **`BEYOND-RIVALS` — Result grouping & facets.** Collapse by repo, language, path, author, or symbol kind; faceted drill-down makes 1000-match results navigable.
- ⚪ **`PARITY` — Commit-pinned permalinks & embeds.** Shareable links with highlighted ranges pinned to a commit; embeddable snippets for docs/PRs.
- 🔵 **`BEYOND-RIVALS` — Rich file experience.** Peek/split view, blame-on-hover, minimap, diff-aware highlighting, "open in editor" deep links (`vscode://`, JetBrains, `nvim`).

---

## Pillar VI — Proactive & Collaborative *(supporting)*

*Search that watches for you, and a team that searches together.*

- 🟢 **`NEW-TO-CATEGORY` — Shift-left search-lint via pre-commit.** Because codebeam sees the **working tree** and installs **git hooks**, a saved query can fire *before* you commit — "you just added a `TODO`, a hardcoded secret, an `httpClient` without a timeout, a banned API." *Why it wins:* Sourcegraph code monitors alert on *new commits, after the fact*; codebeam catches it on *your machine, before it lands*. Search becomes a linter.
- 🔵 **`BEYOND-RIVALS` — Code monitors / alerts (live).** Notify on new matches via Slack/webhook/email — including matches in the working tree, not just committed code.
- 🟢 **`NEW-TO-CATEGORY` — Secret / PII / license scanning as saved structural queries.** Reuse the structural-search engine for a curated, extensible rule pack. One engine, many uses.
- ⚪ **`PARITY` — Team scopes, shared saved searches, bookmarks/annotations.** A collaborative layer on the existing users/permissions model.
- ⚪ **`PARITY` — Search analytics.** Top queries, **zero-result queries** (gap detection), search-to-click rates — which also feed the ranking feedback loop.

---

## Pillar VII — Reach, Ecosystem & Deployment *(supporting)*

*Be everywhere a developer or agent already is — on a laptop and on the org's shared server — with the lowest possible setup cost at every scale.*

- 🔵 **`BEYOND-RIVALS` — `brew install codebeam` → auto-discover local repos → instant search.** Zero config: point it at `~/code` (or let it scan) and it indexes every git repo it finds. Codebeam is a *single binary you run like `ripgrep`* — the moat for the local-first persona.
- 🔵 **`BEYOND-RIVALS` — Same binary, `codebeam serve` for the whole org.** The *identical* artifact boots a multi-user org server: connect code hosts, index everything, and the whole team searches via browser or agent — with **SSO/OIDC/SAML, RBAC, and code-host permission sync** so results honor each person's access. *Why it wins:* incumbents force you into a heavyweight server product; codebeam scales from `ripgrep`-simple to org-wide with one binary and no rearchitecture. *(Fit: a `serve` entrypoint over the existing web/store/indexer packages; the `codehost.Client` + `users`/`repo_permissions` schema already give the foundation.)*
- ⚪ **`PARITY` — First-class CLI/TUI.** `codebeam search`, `codebeam ctx`, an interactive TUI; great terminal ergonomics for the `rg`/`fzf` crowd.
- ⚪ **`PARITY` — REST + GraphQL API.** Codebeam exposes only HTML/HTMX today; a real API unlocks automation, dashboards, and third-party tools.
- 🔵 **`BEYOND-RIVALS` — Editor extensions.** VS Code, JetBrains, Neovim: search your whole org from inside the editor, jump straight to the file — reusing the same local (or server) index.
- ⚪ **`PARITY` — Broaden code hosts.** Add Bitbucket, Gitea, Azure DevOps, Gerrit, sourcehut to reach Sourcebot's coverage. *(The `codehost.Client` abstraction already exists for GitHub/GitLab.)*
- 🟢 **`NEW-TO-CATEGORY` — Instance federation.** Query your *personal* codebeam (local working trees) and your *team* codebeam (shared remote repos) in a single search. The local/remote split becomes a feature, not a limitation.
- 🔵 **`BEYOND-RIVALS` — Optional public index mode.** A read-only, grep.app-style public instance for OSS, and embeddable search widgets for docs sites — community reach that doubles as a live demo.

---

## The Differentiators at a Glance

| Capability | Sourcegraph | Sourcebot | **Codebeam (vision)** |
|---|---|---|---|
| **Live / uncommitted working-tree search** | ❌ (commits only) | ❌ (server snapshots) | 🟢 **Yes — core differentiator** |
| **fsnotify + git-hook auto-update (local)** | ❌ (server poll/webhook) | ❌ (scheduled/webhook) | 🟢 **Yes** |
| **Agent retrieval toolbox over MCP** | partial (AI assistant) | basic MCP (search/read/refs) | 🔵 **Full lexical+structural+symbol+graph+semantic** |
| **Whole-org context for a 1-repo agent** | n/a | partial | 🟢 **Yes, local + private** |
| **Token-budgeted "just enough" retrieval** | ❌ | ❌ | 🟢 **Yes (`codebeam ctx`)** |
| **Zero-config code nav (no SCIP/LSIF)** | ❌ (needs SCIP) | Enterprise-gated | 🔵 **Yes, tree-sitter+ctags default** |
| **Structural / AST search** | deprecated (Comby) | ❌ | 🟢 **Yes, first-class** |
| **Local private semantic (no API key/egress)** | hosted | hosted (BYO key) | 🔵 **Yes, local model** |
| **Always-fresh private codebase wiki** | ❌ | partial (Ask) | 🟢 **Yes, tied to live index** |
| **Shift-left search-lint (pre-commit)** | ❌ (post-commit monitors) | ❌ | 🟢 **Yes** |
| **Single binary: laptop → org cluster** | ❌ (k8s/Docker) | ❌ (Docker) | 🟢 **Yes — same artifact, `codebeam` or `codebeam serve`** |
| **Federate personal (live) + team (org) instances** | ❌ | ❌ | 🟢 **Yes** |
| Org-wide multi-user server (SSO/RBAC/perm-sync) | ✅ | ✅ | ⚪ **Core to server mode (parity + simpler deploy)** |
| Broad code-host coverage | ✅ | ✅ | ⚪ Catch-up (server mode) |
| Multi-branch / symbol search | ✅ | ✅ | ⚙️ **Engine-freebie (unused today)** |
| Saved searches / monitors / analytics | ✅ | ✅ (analytics) | ⚪ Catch-up |

---

## What We Deliberately Protect (the soul)

- **Private by default, in every mode.** Solo: nothing leaves the machine. Server: code, indexes, embeddings, and AI context stay inside your infra/VPC — never a third-party cloud unless you explicitly opt in to a remote model.
- **One binary, zero-to-cluster.** The same artifact runs as a `ripgrep`-simple laptop tool *and* as `codebeam serve` for the whole org — no rewrite, no separate product, no k8s requirement. Every feature degrades gracefully when its optional component (embeddings, SCIP, LLM key, SSO) isn't configured.
- **Fast.** Sub-100ms exact search is the baseline; nothing in the AI/semantic layer is allowed to slow the lexical path.
- **Honest freshness.** The user always knows whether they're searching live, committed, or stale state.

---

## Architecture Implications (light — direction, not sequencing)

Grounded in the current Go + Zoekt + SQLite + HTMX stack, the vision implies a handful of new components:

- **A watcher/sync service** — `fsnotify` watchers, git hooks, webhook receivers, and a scheduler, all feeding the existing `indexer.EnqueueReindex`. (Background workers are new — today everything is request-driven.)
- **A working-tree overlay** in the search path — merge dirty-file matches over committed Zoekt shards, with a `dirty` flag in `search.Result`/`FileMatch`.
- **A structural/semantic layer** — an embedded **ast-grep** engine for AST search (subprocess in v0, then in-process as a WASI WASM module on the existing `wazero` runtime — single binary, no cgo); tree-sitter/ctags for code nav; a local embedding index (new on-disk store) for hybrid search and "find similar."
- **Surface latent Zoekt power** — multi-branch shards and ctags symbol search are already in the engine; expose them in `BuildZoektQuery` and the UI.
- **An MCP server + REST/GraphQL API + CLI** — new front doors alongside the existing web handlers; the search/indexer packages are already cleanly separable.
- **Server/org mode** — `codebeam serve` as a first-class entrypoint on the same binary: multi-user auth (SSO/OIDC, SAML), RBAC, **code-host permission sync** (mirror GitHub/GitLab repo access so search honors it), an admin surface, a shared/scalable index store, and horizontal scale. Extend the existing `users`/`repo_permissions` schema with orgs, teams, saved searches, and monitors. **Federation** lets a solo instance and the team server query each other.

---

## How We'll Know It's Working (success signals & flagship demos)

- **Live working-tree demo:** edit a file without committing → search instantly finds the unsaved change with a `dirty` badge. (Proves Pillar I.)
- **Agent demo:** point Claude Code at codebeam's MCP server while working in one repo → it correctly answers a question about a *different, un-cloned* repo, with `file:line@commit` citations. Measure tokens vs. a `grep`-loop baseline (target: large reduction). (Proves Pillar II.)
- **`codebeam ctx` demo:** one command returns a citation-tagged context bundle that fits a target token budget.
- **Zero-config nav demo:** index a fresh repo in an exotic language → go-to-definition works with no SCIP/language-server setup. (Proves Pillar III.)
- **Structural search demo:** find every empty `catch {}` / error-swallowing block across all repos.
- **Shift-left demo:** add a hardcoded secret → pre-commit hook surfaces it *before* the commit lands. (Proves Pillar VI.)
- **Org-server demo:** two users with different code-host permissions log into one `codebeam serve` instance → both search the whole org, each sees only the repos they're authorized for (permission sync enforced); a federated solo instance then folds the developer's *live working tree* into the same result set. (Proves dual-mode + federation.)
- **Speed/freshness SLOs:** exact search p95 < 100ms; freshness lag (save → searchable) < ~1s for local repos; org server scales to thousands of repos.
- **Adoption signals:** search-to-click rate up, zero-result-query rate down, agent tool-call volume (codebeam replacing in-agent `grep`).

---

## Landscape References

- Sourcebot — features, MCP server, "Ask": https://sourcebot.dev , https://docs.sourcebot.dev/docs/features/mcp-server
- Sourcegraph — code search, code intelligence, code monitoring, deep search: https://sourcegraph.com/docs , https://sourcegraph.com/docs/code-monitoring
- Zoekt — multi-branch + ctags + query AST: https://github.com/sourcegraph/zoekt , https://deepwiki.com/sourcegraph/zoekt
- "Code search for AI agents: the grep replacement is three tools, not one": https://zzet.org/gortex/grep-replacement-for-ai-agents/
- Hybrid (BM25 + embeddings + rerank) consensus: https://mixpeek.com/blog/keyword-vs-semantic-vs-hybrid-search
- ast-grep / tree-sitter structural search: https://ast-grep.github.io
- DeepWiki (auto codebase wiki + MCP): https://docs.devin.ai/work-with-devin/deepwiki
- Local semantic-search MCPs (Claude Context, CodeGrok, Serena): https://github.com/oraios/serena
