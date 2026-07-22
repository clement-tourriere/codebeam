---
title: AI agents and APIs
description: Connect Claude Code and other MCP clients, pack context with the CLI, or call the JSON API.
---

Codebeam was built with AI coding agents in mind. An agent that greps a checkout burns context on false positives and can't see repositories it hasn't cloned; Codebeam gives it the opposite — precise, indexed retrieval over *everything*, with every result carrying a `repo:path:line@commit` citation the agent can verify.

Four interfaces expose the same index:

- the **MCP server** — the richest one; use this for Claude Code and other agents,
- the **[`cb` CLI](/codebeam/cli/)** — the same tools from any terminal, with the same auth; the easiest path for shell-based agents, CI, and humans,
- the **`codebeam ctx` CLI** — one command that packs cited context for a prompt (local index only),
- the **JSON API** — for scripts and custom tooling.

## MCP server

The Model Context Protocol server gives agents eight retrieval tools:

| Tool | What the agent gets |
| --- | --- |
| `search_code` | Lexical/regex search with cited snippets. |
| `symbol_search` | Go to definition — where a symbol is *defined*. |
| `find_references` | Word-boundary usages of a symbol. |
| `structural_search` | AST-pattern search (`$VAR`, `$$$` metavariables) with captured variables reported. |
| `read_file` | A bounded line range of a file, with commit provenance. |
| `file_tree` | The file/directory listing of a repository. |
| `list_repos` | Which repositories are indexed, and how fresh. |
| `repo_stats` | Language breakdown, file/line counts, last commit, indexed branches. |

Together these replace the grep → read → grep loop with targeted queries — typically a large token saving per question, and answers grounded in code the agent couldn't otherwise see.

All four search-like tools understand facets. Set `facets: true` to receive compact repository/language/path/provider/etc. counts, include several repositories with `repos`, and remove noisy buckets with arrays such as `exclude_repos`, `exclude_langs`, `exclude_top_paths`, or `exclude_extensions`. The same positive and negative facet model is used by the web UI, API, and `cb`.

### Local agents: stdio

For an agent running on the same machine, use the stdio transport. One command for Claude Code:

```sh
claude mcp add codebeam -- codebeam mcp
```

Any MCP client that takes a JSON config works the same way:

```json
{
  "mcpServers": {
    "codebeam": {
      "command": "codebeam",
      "args": ["mcp"]
    }
  }
}
```

`codebeam mcp` reads the same database and index as the web app. It needs no authentication (it's your own local process) and sees every indexed repository. If you moved the data directory, pass the same environment (e.g. `"env": {"CODEBEAM_DATA_DIR": "/var/lib/codebeam"}`).

### Remote agents: streamable HTTP

A shared Codebeam deployment serves MCP over HTTP at `POST /mcp`, authenticated and **scoped to the repositories the authenticated user can access** — so a team server honors per-user permissions.

Codebeam is a full OAuth 2.1 authorization server with dynamic client registration, which means MCP clients set themselves up:

```sh
claude mcp add --transport http codebeam https://codebeam.example.com/mcp
```

Claude Code discovers the OAuth flow from the server's `/.well-known` metadata, opens a consent page in your browser, and stores the tokens. Grants are listed — and revocable — under **Settings → Connected agents**.

Scripted clients can skip OAuth and send a [personal access token](/codebeam/oauth/#codebeam-api-tokens) instead:

```sh
curl -X POST https://codebeam.example.com/mcp \
  -H "Authorization: Bearer cbp_…" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_repos"}}'
```

When the instance sits behind an SSO gateway like Cloudflare Access — where a direct HTTP connection never reaches Codebeam — register [`cb mcp`](/codebeam/cli/#one-command-for-agents-cb-mcp) instead: a stdio proxy to the same `/mcp` endpoint that reuses `cb login`'s credentials, gateway hop included:

```sh
claude mcp add codebeam -- cb mcp
```

## Packed-context CLI

`codebeam ctx` answers a question with a ready-to-paste context bundle: the most relevant files, trimmed to a character budget, each chunk cited as `repo:path@commit`. Pipe it into a prompt, or let a script call it before invoking a model:

```sh
codebeam ctx "how is auth wired?" --repo codebeam --lang go
```

| Flag | Default | Purpose |
| --- | --- | --- |
| `--repo` | *(all)* | Restrict to one repository. |
| `--path` | *(all)* | Restrict to paths matching a pattern. |
| `--lang` | *(all)* | Restrict to a language. |
| `--max-files` | `10` | Maximum files to include. |
| `--max-chars` | `12000` | Approximate output budget in characters. |

Like the stdio MCP server, `ctx` reads the local data directory — set `CODEBEAM_DATA_DIR` to match the server if you moved it.

## JSON API

Two endpoints, served by the web process. Browser sessions work for same-origin calls; programmatic callers send `Authorization: Bearer cbp_…` with an [API token](/codebeam/oauth/#codebeam-api-tokens).

### `GET /api/search`

```http
GET /api/search?q=handler&repo=local/codebeam&lang=go
```

| Parameter | Purpose |
| --- | --- |
| `q` | The query (required). Regex by default — same [syntax as the web UI](/codebeam/searching/). |
| `mode=structural` | Structural (AST) search; `q` becomes an ast-grep pattern and `lang` is required. |
| `sym=1` | Symbol definitions only. |
| `norm=1` | Normalized matching (case- and accent-insensitive). |
| `repo` | Repository filter; repeat the parameter to OR several. |
| `branch`, `path`, `top`, `ext`, `lang` | Branch, path substring, top-level path, extension, language. |
| `source`, `provider` | Local vs. remote; code-host filter. |
| `dirty`, `freshness`, `symbol_kind` | Working-tree state, index age bucket, definition kind. |
| `sort` | Relevance (default), repo, path, index age, or match count. |
| `exclude_repo` | Repository to exclude; repeat to exclude several. |
| `exclude_branch`, `exclude_top`, `exclude_ext`, `exclude_lang` | Repeatable negative branch/path/extension/language facets. |
| `exclude_source`, `exclude_provider` | Repeatable negative source/provider facets. |
| `exclude_dirty`, `exclude_freshness`, `exclude_symbol_kind` | Repeatable negative working-tree/freshness/symbol facets. |

The response reports which `engine` ran (`zoekt` or `structural`), the compiled `engine_query`, a `truncated` flag when any cap cut the results, facet counts, and the matched files. Each facet value has `active: true` when included or `excluded: true` when negatively selected. Each file carries the `commit` it was indexed at (empty for non-git sources) and a `dirty` flag; structural matches also include a `meta_vars` object with the text captured by each `$VAR`.

### `GET /api/read`

```http
GET /api/read?repo=local/codebeam&path=internal/config/config.go&start=1&end=80
```

Returns a bounded line range (up to 500 lines per call). Add `branch=` to read a file as it exists on any indexed remote branch.

## VS Code extension

A minimal extension lives in `extensions/vscode`: it adds a **Codebeam: Search Selection** command that opens the selected text in your Codebeam instance. Build it from that directory with `npm install && npm run compile`, and point its setting at your Codebeam URL.
