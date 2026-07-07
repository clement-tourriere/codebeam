# Codebeam

Codebeam is a local-first Sourcegraph-style code search prototype backed by Zoekt.

## Install

Released binaries are fully self-contained (web UI assets included). Pick one:

**Install script** (Linux and macOS, amd64/arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
codebeam            # web UI on http://localhost:8080
```

**Docker**:

```sh
docker run -d --name codebeam -p 8080:8080 \
  -v codebeam-data:/data -v codebeam-config:/config \
  ghcr.io/clement-tourriere/codebeam:latest
```

**Runtime requirements**: `git` must be on `PATH` — Codebeam shells out to it to clone, fetch, and read repositories (the Docker image ships it). [Universal Ctags](https://github.com/universal-ctags/ctags) is optional and enables symbol search.

Binaries for each platform are also on the [releases page](https://github.com/clement-tourriere/codebeam/releases).

## CLI

The release archive also ships `cb`, a client for any Codebeam server — the same cited, permission-scoped results the MCP tools return, from any terminal:

```sh
cb login codebeam.example.com     # one-time browser sign-in (OAuth, like an MCP client)
cb search "func NewServer" --lang go
cb def NewServer                  # where is it defined?
cb read local/app:cmd/main.go:1-40
```

Agents and CI skip the login entirely: set `CODEBEAM_URL` and `CODEBEAM_TOKEN` (a personal access token from **Settings → API tokens**) and every command works headless. See the [CLI documentation](https://clement-tourriere.github.io/codebeam/cli/).

## Documentation

The full setup and operations documentation is published at **<https://clement-tourriere.github.io/codebeam/>** (OAuth, environment variables, indexing, integrations, deployment). It lives in the Astro Starlight site under `docs/`:

```sh
mise run docs:dev
mise run docs:build
```

## Develop

Use `mise` to install the pinned toolchain and run the app. The dev task installs missing frontend npm dependencies automatically:

```sh
mise install
mise run dev
```

The app defaults to `http://localhost:8080` and stores data under `.codebeam/`.

## OAuth

Local development login is enabled by default. To connect real code hosts with OAuth, create an OAuth app on the host and put its credentials in `.env` (loaded automatically by `mise`):

```sh
cp .env.example .env
$EDITOR .env
mise run dev
```

GitHub OAuth app setup:

1. Open GitHub → **Settings** → **Developer settings** → **OAuth Apps** → **New OAuth App**.
2. Use:
   - **Application name:** `Codebeam`
   - **Homepage URL:** `http://localhost:8080`
   - **Authorization callback URL:** `http://localhost:8080/auth/github/callback`
3. Copy the client ID and generate a client secret into `.env`:

```dotenv
CODEBEAM_BASE_URL=http://localhost:8080
CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...
```

GitLab callback URL: `http://localhost:8080/auth/gitlab/callback`

GitHub can also be connected from the Sources page with a personal access token, which avoids configuring OAuth env vars for local use.

For self-managed GitLab, either set `CODEBEAM_GITLAB_BASE_URL` before starting the app for OAuth, or sign in locally and use the "Self-managed GitLab" card on the Repositories page with a personal access token that has `read_api` and `read_repository`.

## V0 Flow

1. Log in locally or with GitHub/GitLab OAuth.
2. Paste a public GitHub URL, sync GitHub/GitLab repositories, or add a local repository path.
3. Select repositories and run indexing (public GitHub repos can be added and indexed without OAuth).
4. Search with Zoekt query syntax plus repo/branch/path/language filters and result facets.
5. Open results in the code viewer.

## Live local freshness

Selected local repositories are watched by default. Codebeam queues a debounced background reindex when files change on disk, so local search stays fresh without pressing the reindex button. Local search results also show a `dirty` badge when the matching file has uncommitted changes. Set `CODEBEAM_WATCH_LOCAL_REPOS=false` to disable the watcher.

## Remote auto-indexing

Selected remote repositories (public GitHub, GitHub OAuth, GitLab, self-managed GitLab) are re-pulled and reindexed on a schedule, so their search stays fresh without manual reindexing. A background scheduler periodically enqueues a reindex for any selected remote repo whose last index attempt is older than the refresh interval — which also picks up newly-selected repos and naturally backs off ones that recently failed. Private repos fetch with the token of a user who has access; public GitHub repos work without a token, so they can be added from the Sources page by URL.

- `CODEBEAM_REMOTE_REFRESH_INTERVAL` (default `30m`) — how stale a repo may get before it is refreshed. Accepts Go durations (`15m`, `1h`, …).
- `CODEBEAM_AUTO_INDEX_REMOTE=false` — disable the scheduler.
- `CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE` (default `true`) — when a remote repo fails to index with an access/permission error (e.g. a GitLab project the token can't read), drop it from the selection so the scheduler stops retrying it. Re-select it once access is restored. Overridable on the `/settings` page.
- `CODEBEAM_INDEX_CONCURRENCY` — maximum repository indexing jobs to run at once (default: bounded by CPU, up to 4).
- `CODEBEAM_INDEX_FILE_CONCURRENCY` — per-repository document/blob reader concurrency (default: bounded by CPU, up to 8).

## Branch indexing

Remote repositories can index one, many, or all branches. Leave the branch field empty to use the host's default branch; set comma-separated names or glob patterns to include branches (`main,release/*`); use `*`/`all` for every remote branch; and prefix patterns with `!` to exclude branches (`*,!wip/*,!dependabot/*`). Codebeam records the resolved branch set on each successful index, so deleted branches disappear on the next refresh; if an explicitly requested branch is missing, the repo is marked as needing reindex instead of silently serving stale results. Search facets expose indexed branches, and `branch:`/`branch=` filters scope searches to a branch. Search results deduplicate equivalent hits across branches and show branch locations as badges with `+N` overflow.

When a branch pattern (such as `*`) matches more branches than `CODEBEAM_MAX_INDEXED_BRANCHES` (default `20`), Codebeam indexes only the most recently updated branches — the default branch is always kept — instead of cloning and indexing thousands of stale branches. Ranking uses a metadata-only fetch of branch tips, so it stays cheap even on repos with many branches. Admins can change the limit on the `/settings` page; the search index (Zoekt) supports at most **64 branches per repository**, so values above 64 (or `0`) mean that maximum. A selection of explicitly named branches that exceeds the limit fails with a clear error instead of silently dropping branches.

## Agent-facing retrieval

- `GET /api/search?q=...` returns JSON search results and facets. Authenticate with a browser session or `Authorization: Bearer <token>` using a personal access token from **Settings → API tokens**. It accepts the same `repo`, `branch`, `path`, `top`, `ext`, `lang`, `source`, `provider`, `dirty`, `symbol_kind`, `freshness`, and `sort` controls as the web UI. Repeat `repo` to search multiple repositories with boolean OR. Add `mode=structural&lang=<language>` to run a structural (AST) search instead of a lexical one; the response then carries `engine: "structural"` and per-line `meta_vars` captures.
- `GET /api/read?repo=local/repo&path=main.go&start=1&end=80` returns a bounded JSON file range; add `branch=...` for indexed remote branches.
- `codebeam ctx "how is auth wired?"` prints a citation-style context bundle from the local indexed repos. Use `--max-chars`, `--max-files`, `--repo`, `--path`, and `--lang` to control the bundle.

## Authentication & roles

The first user to sign in becomes the instance **admin** (settings, local repository paths, user management); later users join as **members** and see only repositories they have access to. Company deployments can plug in OIDC single sign-on (Okta, Entra ID, Google Workspace, Keycloak, …) with an optional email-domain allowlist — set `CODEBEAM_OIDC_ISSUER`/`CLIENT_ID`/`CLIENT_SECRET`. Agents and scripts authenticate with personal access tokens (`Authorization: Bearer cbp_…`), and MCP clients can walk a full OAuth 2.1 flow (dynamic client registration + PKCE) against the built-in authorization server — point them at `/mcp` and approve the browser consent page. See the integrations doc for details.

## MCP server

`codebeam mcp` runs a Model Context Protocol server over stdio so coding agents can use your local Codebeam index as a retrieval toolbox instead of `grep`. A shared deployment additionally serves the streamable-HTTP transport at `POST /mcp`, bearer-authenticated and scoped to each user's repositories (`claude mcp add --transport http codebeam https://your-host/mcp`). It exposes eight tools backed by the same engines the web UI uses:

- `search_code` — lexical/regex search across every indexed repository, returning ranked snippets with `repo:path:line` citations and a `dirty` flag for uncommitted working-tree matches. Accepts `query`, `repo`, `path`, `lang`, and `max_results`.
- `structural_search` — AST-shape search with ast-grep patterns (`$VAR` captures one node, `$$$` any number), e.g. `if $ERR != nil { $$$ }`. Runs fully in-process (embedded WASM engine, no external binary) and reports captured metavariables with each match. Accepts `pattern`, `lang` (required), `repo`, `branch`, `path`, and `max_results`.
- `symbol_search` — find where a symbol is **defined** (function, type, method, …) via Zoekt's ctags-backed symbol index, instead of every textual mention. Accepts `symbol`, `repo`, `lang`, and `max_results`. Requires symbol indexing (see below).
- `find_references` — find **usages** of a symbol with word-boundary precision (so `Get` does not also match `Getter`); the companion to `symbol_search` for code navigation. Accepts `symbol`, `repo`, `lang`, and `max_results`.
- `read_file` — a bounded line range from an indexed file, with provenance. Accepts `repo`, `path`, `start`, `end`.
- `file_tree` — list files and directories in a repository (optionally under a sub-path) to orient before reading or searching. Accepts `repo`, `path`, and `max_entries`.
- `list_repos` — the repositories that are currently indexed and searchable.
- `repo_stats` — per-repository statistics with fleet-wide totals: language breakdown (percentages by bytes), file/line/size counts, last commit date, and indexed branches/commit. Covers the primary branch's indexed content, so numbers match what search sees.

Register it with Claude Code:

```sh
claude mcp add codebeam -- codebeam mcp
```

or add it to any MCP client config (for example `.mcp.json` / Cursor):

```json
{
  "mcpServers": {
    "codebeam": { "command": "codebeam", "args": ["mcp"] }
  }
}
```

The server reads the same `.codebeam/` data the app indexes, so point it at a custom data dir with `CODEBEAM_DATA_DIR` (or `CODEBEAM_DB_PATH` / `CODEBEAM_INDEX_DIR`) if you moved it.

## Symbol search

Codebeam can search **symbol definitions** (functions, types, methods, constants, …) rather than raw text, across 100+ languages:

- In the web UI, tick **Symbols only** on the search form.
- Over the JSON API, add `&sym=1` to `/api/search`.
- For agents, use the `symbol_search` MCP tool.

The file viewer also shows a **Symbols** outline for the open file (jump to any definition; "refs" links run a word-boundary find-references search scoped to the repo). The outline uses live ctags, so it reflects the file on disk even before re-indexing. Identifiers in the code body that are defined in the same file are **click-to-definition** links (they keep their syntax colour and only underline on hover).

Symbol search needs [Universal Ctags](https://github.com/universal-ctags/ctags) to be available **when a repository is indexed**. Codebeam auto-detects `universal-ctags` (or a `ctags` that is really Universal Ctags) on your `PATH`. On macOS the system `/usr/bin/ctags` is BSD ctags, which does not work, and Homebrew installs Universal Ctags keg-only, so point Codebeam at it explicitly and re-index:

```sh
brew install universal-ctags
export CODEBEAM_CTAGS_PATH="$(brew --prefix universal-ctags)/bin/ctags"
mise run dev   # then re-index repositories so symbols are captured
```

When no Universal Ctags binary is configured, symbol search simply returns no matches — every other feature is unaffected.

## Structural search

Codebeam can search code by **AST shape** instead of text, using [ast-grep](https://ast-grep.github.io/) patterns — find `if $ERR != nil { $$$ }` regardless of variable names or formatting:

- In the web UI, tick **Structural (AST)** on the search form and pick a language.
- Over the JSON API, add `&mode=structural&lang=go` to `/api/search`.
- For agents, use the `structural_search` MCP tool.

Patterns are ordinary code plus metavariables: `$VAR` captures one AST node, `$$$` captures any number (`$$$ARGS` to name the capture). The pattern must itself parse as valid code in the chosen language. Captured metavariables are shown with each match.

The engine is **built into the Codebeam binary**: ast-grep's Rust core and its tree-sitter grammars are compiled to a WebAssembly module (see `wasm/astgrep/`) and executed in-process on wazero. Nothing to install, no subprocess, `CGO_ENABLED=0` and the single-binary deploy story are preserved. Bundled languages: bash, c, go, java, javascript, json, python, rust, tsx, typescript, yaml.

Structural search scans the **live worktree** for local repositories (uncommitted changes included) and the primary indexed branch for remote clones. Pass `branch=<name>` to search any other indexed branch — file contents are read straight from the git object store, without touching the working tree. Scans are bounded by `CODEBEAM_STRUCTURAL_TIMEOUT` (15s), `CODEBEAM_STRUCTURAL_MAX_FILES` (5000), and `CODEBEAM_STRUCTURAL_MAX_MATCHES` (1000); truncated results are flagged. It is deliberately a separate, slower tool than lexical search (roughly 5–7 ms per file) — narrow with repo/path filters for large sweeps.

To rebuild the WASM engine after changing `wasm/astgrep/` (requires Rust with the `wasm32-wasip1` target; wasi-sdk is fetched automatically):

```sh
mise run wasm:build
```

## VS Code Extension

The extension scaffold lives in `extensions/vscode`. It adds `Codebeam: Search Selection` and opens the selected text in the configured Codebeam instance.

## Releasing

Versioning follows [Conventional Commits](https://www.conventionalcommits.org/) via [commitizen](https://commitizen-tools.github.io/commitizen/) (installed by `mise install`). Git tags are the single source of truth for the version — the binary gets it injected at build time.

```sh
mise run release
```

This runs `cz bump` (computes the next semver from the commit history, updates `CHANGELOG.md`, creates the `vX.Y.Z` tag) and pushes with `--follow-tags`. The pushed tag triggers the release workflow, which:

- cross-compiles self-contained binaries for linux/darwin × amd64/arm64 with the version injected,
- publishes them (plus SHA-256 checksums) as a GitHub Release with generated notes, and
- builds and pushes the multi-arch Docker image to `ghcr.io/clement-tourriere/codebeam` (`latest`, `X.Y`, `X.Y.Z`).

## License

[MIT](LICENSE). Codebeam depends on permissively licensed components, notably [Zoekt](https://github.com/sourcegraph/zoekt) (Apache-2.0), [wazero](https://github.com/tetratelabs/wazero) (Apache-2.0), [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (BSD-3-Clause), and [ast-grep](https://github.com/ast-grep/ast-grep) (MIT).
