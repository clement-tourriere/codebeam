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

## Behind Cloudflare Access

Company instances are often fronted by [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/) (Zero Trust SSO via Okta, Entra ID, …), which intercepts every request before it reaches Codebeam. `cb` handles this by itself: `cb login` detects the gateway, sends your browser through the identity provider, and from then on attaches the Access token to every request automatically — no flags, no configuration.

The browser hand-off uses Cloudflare's own `cloudflared` CLI, so it must be installed once:

```sh
# macOS
brew install cloudflared

# Linux (any distro, direct binary; use arm64 instead of amd64 on ARM)
sudo curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 \
  -o /usr/local/bin/cloudflared && sudo chmod +x /usr/local/bin/cloudflared
```

Debian/RPM packages are also available from [pkg.cloudflare.com](https://pkg.cloudflare.com/). If `cloudflared` is missing, `cb login` says exactly that instead of failing cryptically.

After login, nothing else changes: search commands reuse `cloudflared`'s cached token silently, and `cb status` shows a `Gate: Cloudflare Access` line. When the Access session eventually expires (your company's SSO session policy), any command tells you to run `cb login` again — `cb` never opens a browser on its own outside of `login`, so scripts and agents get a clear error rather than a surprise window.

Headless machines (CI, agents) skip `cloudflared` entirely: ask your Cloudflare administrator for a [service token](https://developers.cloudflare.com/cloudflare-one/identity/service-tokens/) and export it —

```sh
export CF_ACCESS_CLIENT_ID=….access
export CF_ACCESS_CLIENT_SECRET=…
```

combined with `CODEBEAM_TOKEN`, every command works with no login and no browser. Servers that are not behind Cloudflare Access are untouched by all of this — detection happens once at login and changes nothing when no gateway answers.

## One command for agents: cb mcp

`cb mcp` serves the MCP stdio transport and relays every message to the server's `/mcp` endpoint with cb's stored credentials — OAuth refresh and the Cloudflare Access hop included. Any MCP client gets remote, permission-scoped retrieval with a one-line registration, no headers or tokens to manage:

```sh
cb login codebeam.example.com        # once
claude mcp add codebeam -- cb mcp    # or any client config: command "cb", args ["mcp"]
```

This is the way through for agents when the instance sits behind an SSO gateway (they cannot drive `cloudflared` themselves), and it works equally well on plain instances. It targets the default server (the last `cb login`); pass `--server` to pin one, or set `CODEBEAM_URL` + `CODEBEAM_TOKEN` for fully headless use. Failures — including an expired SSO session — come back as JSON-RPC errors inside the agent's conversation ("run `cb login …`"), so the agent can tell you how to fix it instead of silently losing its tools.

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

`cb search` exposes the web UI's facets from the terminal. Positive filters include `--repo` (repeat it to OR repositories), `--branch`, `--top-path`, `--ext`, `--lang`, `--source`, `--provider`, `--dirty`, `--symbol-kind`, and `--freshness`. Add the `--exclude-` prefix to remove buckets — for example `--exclude-repo`, `--exclude-lang`, or `--exclude-top-path`; every exclusion flag is repeatable. `--path` remains a regex filter, `--sort` changes ordering, and `-n` caps returned files. `cb ast` exposes the relevant repository/path/source/working-tree facets; `cb def` and `cb refs` accept repeatable repository and language includes/excludes.

Pass `--facets` to print compact counts with the results. This makes an iterative shell workflow possible: run a broad faceted search, then copy the shown raw values into include/exclude flags. Every command accepts `--server` to target a specific instance, and flags may come before or after the arguments.

`cb read` accepts a pasted citation unchanged — `cb read local/app:cmd/main.go@1a2b3c4d:12-40` reads lines 12–40, ignoring the commit suffix search results print.

## Examples

```sh
# Where is retry logic implemented, Go only, top 5 files?
cb search "retry backoff" --lang go -n 5

# Inspect facets, then remove noisy repositories and non-code languages
cb search "TODO" --facets
cb search "TODO" --exclude-repo local/vendor --exclude-lang Markdown --facets

# Search two repos together while excluding generated code
cb search "NewServer" --repo local/api --repo local/worker --exclude-top-path generated

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
