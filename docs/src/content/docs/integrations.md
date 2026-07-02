---
title: Integrations and APIs
description: Use Codebeam from agents, scripts, MCP clients, the CLI, and VS Code.
---

Codebeam exposes the same local index through several interfaces.

## Web UI

Open `/search` to search indexed repositories. The UI supports Zoekt query syntax plus filters for repository, branch, path, language, provider, freshness, dirty state, and symbols.

Tick **Structural (AST)** and pick a language to switch the form to structural search: the query becomes an [ast-grep](https://ast-grep.github.io/) pattern (`$VAR` captures one AST node, `$$$` any number), for example `if $ERR != nil { $$$ }`. Matches show the captured metavariables, and the facet sidebar works the same way as in text mode.

## JSON API

The JSON API is served by the web process and uses the same authenticated browser session as the UI.

## Authentication

Programmatic callers authenticate with a bearer credential in the `Authorization` header:

- **Personal access tokens** — create one under **Settings → API tokens** (`cbp_…`). Send it as `Authorization: Bearer cbp_…` on `/api/*` and `/mcp`. Tokens carry your repository permissions and can expire or be revoked.
- **OAuth 2.1 (MCP clients)** — Codebeam is a full OAuth 2.1 authorization server with dynamic client registration, so MCP clients discover the flow automatically: point one at `/mcp`, approve the consent page in your browser, done. Grants are listed and revocable under **Settings → Connected agents**.

Browser sessions (cookies) keep working for same-origin calls. Unauthenticated requests get `401` with a `WWW-Authenticate` header pointing at `/.well-known/oauth-protected-resource/mcp` (RFC 9728), which is how MCP clients find the authorization server.

Two roles govern the UI: the first user becomes the **admin** (instance settings, local repository paths, user management), everyone else joins as a **member**. Admins manage roles under **Settings → Users**; the last admin cannot be demoted.

Repository access follows the code host. Each user only sees the repositories they have permission for, and Codebeam mirrors GitHub/GitLab access automatically: a background sync grants access a user newly gained and revokes access they lost on a private repo, so search stays aligned with the host without a manual re-sync (public repos are never pruned). See `CODEBEAM_SYNC_PERMISSIONS` in the configuration reference.

### Search

```http
GET /api/search?q=handler&repo=local/codebeam&branch=main&lang=go
```

Common parameters:

| Parameter | Purpose |
| --- | --- |
| `q` | Search query. |
| `repo` | Repository filter. Repeat it to OR multiple repositories. |
| `branch` | Indexed branch filter. |
| `path` | Path substring or pattern filter. |
| `top` | Top-level path filter. |
| `ext` | File extension filter. |
| `lang` | Language filter. |
| `source` | Source type filter. |
| `provider` | Code host/provider filter. |
| `dirty` | Filter dirty/local freshness results. |
| `symbol_kind` | Filter symbol definition kind. |
| `freshness` | Filter by age bucket. |
| `sort` | Sort mode. |
| `sym=1` | Search symbol definitions instead of full file text. |
| `mode=structural` | Run a structural (AST) search instead of a lexical one. Requires `lang`; `q` is an ast-grep pattern. |

Each matched file in the response carries a `commit` field: the hash the repository was at when it was last indexed, so agents can pin a citation to `repo:path:line@commit` and verify it against source. It is empty for non-git sources, and a `dirty: true` file may differ from that commit. The MCP tools and `codebeam ctx` render the same provenance inline as `repo:path@<short-commit>`.

### Structural search

```http
GET /api/search?mode=structural&lang=go&q=if%20%24ERR%20!%3D%20nil%20%7B%20%24%24%24%20%7D
```

Structural mode honors the `repo`, `branch`, `path`, `top`, `ext`, `source`, `provider`, `dirty`, and `freshness` filters. The response sets `engine` to `"structural"` (`"zoekt"` for lexical searches), echoes the pattern in `engine_query`, flags partial results with `truncated` (both engines set it when a file/match cap, the timeout, or the display window cut the result list), and adds a `meta_vars` object to each matched line with the text captured by every `$VAR`/`$$$VAR`. Supported languages: bash, c, go, java, javascript, json, python, rust, tsx, typescript, yaml.

Structural search scans the live working tree for local repositories (uncommitted changes included, badged with `dirty: true` per file) and the checked-out primary branch for remote clones; pass `branch=<name>` to search any other indexed branch's committed state.

### Read a file range

```http
GET /api/read?repo=local/codebeam&path=internal/config/config.go&start=1&end=80
```

Add `branch=...` for indexed remote branches.

## MCP server

Codebeam speaks MCP over two transports:

- **stdio** (`codebeam mcp`) — the local, solo-mode transport. Runs on your machine, sees every indexed repository, no authentication (it is your own process).
- **streamable HTTP** (`POST /mcp`) — the server-mode transport. Bearer-authenticated (OAuth 2.1 or a personal access token) and scoped to the repositories the authenticated user can access, so a shared deployment honors per-user permissions.

### Streamable HTTP

Register the remote endpoint with Claude Code — it discovers the OAuth flow via the `/.well-known` metadata, opens the consent page in your browser, and stores the tokens:

```sh
claude mcp add --transport http codebeam https://codebeam.example.com/mcp
```

Scripted clients can skip OAuth and send a personal access token instead:

```sh
curl -X POST https://codebeam.example.com/mcp \
  -H "Authorization: Bearer cbp_…" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_repos"}}'
```

### stdio

`codebeam mcp` runs the stdio server. It reads the same database and Zoekt index as the web app, so use the same data-path environment if you moved `CODEBEAM_DATA_DIR`.

```sh
codebeam mcp
```

Register with Claude Code:

```sh
claude mcp add codebeam -- codebeam mcp
```

For clients that use JSON configuration:

```json
{
  "mcpServers": {
    "codebeam": {
      "command": "codebeam",
      "args": ["mcp"],
      "env": {
        "CODEBEAM_DATA_DIR": "/var/lib/codebeam"
      }
    }
  }
}
```

Available MCP tools:

| Tool | Purpose |
| --- | --- |
| `search_code` | Lexical/regex search over indexed repositories. |
| `structural_search` | Structural (AST) search with ast-grep patterns; reports captured metavariables. Requires `pattern` and `lang`. |
| `symbol_search` | Find symbol definitions through Zoekt's ctags-backed symbol index. |
| `find_references` | Find word-boundary symbol references. |
| `read_file` | Read a bounded file range with provenance. |
| `file_tree` | List files and directories in a repository. |
| `list_repos` | List indexed/searchable repositories. |
| `repo_stats` | Per-repository statistics: language breakdown, file/line/size counts, last commit date, indexed branches/commit. |

## Packed-context CLI

`codebeam ctx` prints a citation-style context bundle for coding agents:

```sh
codebeam ctx "how is auth wired?" --repo codebeam --lang go --max-files 8 --max-chars 20000
```

Useful flags include:

- `--max-chars`
- `--max-files`
- `--repo`
- `--path`
- `--lang`

## VS Code extension

The VS Code extension scaffold lives in `extensions/vscode`. It adds **Codebeam: Search Selection** and opens selected text in the configured Codebeam instance.

Install dependencies and build the extension from that directory when working on it:

```sh
cd extensions/vscode
npm install
npm run compile
```

Configure the extension to point at your Codebeam web URL, for example `http://localhost:8080` or `https://codebeam.example.com`.

## Custom data directories

All non-HTTP integrations must read the same data as the web server. If the web app uses custom paths, set the same variables for CLI/MCP invocations:

```sh
CODEBEAM_DATA_DIR=/var/lib/codebeam codebeam mcp
CODEBEAM_DATA_DIR=/var/lib/codebeam codebeam ctx "where is indexing triggered?"
```

You can also set `CODEBEAM_DB_PATH` and `CODEBEAM_INDEX_DIR` individually.
