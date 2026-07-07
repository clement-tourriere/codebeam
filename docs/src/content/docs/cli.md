---
title: The cb CLI
description: Search your indexed repositories from any terminal — browser login for humans, a token variable for agents and CI.
---

`cb` brings Codebeam's index to the command line. It talks to a Codebeam server over the same authenticated `/mcp` endpoint AI agents use, so it returns the same cited, permission-scoped results — from any machine that can reach the server, with nothing but a single binary.

```sh
cb login codebeam.example.com     # once — opens your browser
cb search "func NewServer" --lang go
```

It is built for both audiences:

- **Humans** get a one-time browser login and short commands (`cb search`, `cb def`, `cb read`).
- **Agents and CI** get zero-setup auth: set `CODEBEAM_TOKEN` to a [personal access token](/codebeam/oauth/#codebeam-api-tokens) and every command just works — no login, no config file.

## Install

`cb` ships in the same release archive as the server, so the [install script](/codebeam/getting-started/) already puts it on your `PATH`. The Docker image includes it too (`docker exec <container> cb …`).

## Sign in

```sh
cb login                          # local server (http://localhost:8080)
cb login codebeam.example.com     # team server
```

`cb login` runs the exact OAuth 2.1 flow an MCP client runs: it registers itself with the server, opens your browser to the consent page, and stores the tokens (with automatic refresh) in `~/.config/codebeam/cli.json`. The grant shows up — and can be revoked — under **Settings → Connected agents**, like any other agent.

No browser on that machine (SSH box, container, CI)? Use a personal access token from **Settings → API tokens**:

```sh
cb login codebeam.example.com --token cbp_…   # store it, or:
export CODEBEAM_URL=https://codebeam.example.com
export CODEBEAM_TOKEN=cbp_…                   # no login at all
```

`cb status` shows which server and credentials a command would use; `cb logout` forgets a server's stored credentials.

## Commands

| Command | What you get |
| --- | --- |
| `cb search <query>` | Lexical/regex search — full [Zoekt syntax](/codebeam/searching/) (`file:`, `lang:`, `sym:`, boolean operators). |
| `cb ast '<pattern>' --lang <l>` | Structural (AST) search with ast-grep metavariables (`$VAR`, `$$$`). |
| `cb def <symbol>` | Where a symbol is defined (ctags-backed). |
| `cb refs <symbol>` | Word-boundary usages of a symbol. |
| `cb read <repo:path[:N-M]>` | A bounded line range, with commit provenance. |
| `cb tree <repo> [path]` | A repository's file listing. |
| `cb repos` | What is indexed, and how fresh. |
| `cb stats` | Languages, sizes, freshness per repository. |

Search commands share the filters `--repo`, `--path`, `--lang` and `-n` (max files); `cb ast` adds `--branch`. Every command accepts `--server` to target a specific instance, and flags may come before or after the arguments.

`cb read` accepts a pasted citation unchanged — `cb read local/app:cmd/main.go@1a2b3c4d:12-40` reads lines 12–40, ignoring the commit suffix search results print.

## Examples

```sh
# Where is retry logic implemented, Go only, top 5 files?
cb search "retry backoff" --lang go -n 5

# Every Go error-swallowing pattern in one repo
cb ast 'if $ERR != nil { return nil }' --lang go --repo local/app

# Chase a symbol: definition, then callers
cb def NewServer
cb refs NewServer --lang go

# Read around a hit (copied straight from search output)
cb read local/app:internal/web/server.go:290-340

# An agent or CI job, fully headless
CODEBEAM_URL=https://codebeam.example.com CODEBEAM_TOKEN=cbp_… \
  cb search "TODO(security)" -n 20
```

Output is plain, citation-tagged text — the same compact format the MCP tools return — so it pipes cleanly into prompts, scripts, and pagers.

## Several servers

Credentials are stored per server URL. `--server` (or `CODEBEAM_URL`) picks the target per command; the most recent `cb login` sets the default. `CODEBEAM_CLI_CONFIG` relocates the credentials file when `$HOME` isn't writable (containers, CI sandboxes).

## cb or codebeam ctx?

[`codebeam ctx`](/codebeam/integrations/#packed-context-cli) reads the local data directory directly and packs a character-budgeted context bundle; it only works on the machine that hosts the index. `cb` goes through the server with real authentication and per-user repository scoping — it works from anywhere and is the right tool for teams, agents, and CI.
