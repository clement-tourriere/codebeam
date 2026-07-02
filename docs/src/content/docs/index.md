---
title: Codebeam instance guide
description: Set up, configure, and operate a Codebeam code search instance.
---

Codebeam is a local-first, Sourcegraph-style code search service backed by [Zoekt](https://github.com/sourcegraph/zoekt), with structural (AST) search powered by an embedded [ast-grep](https://ast-grep.github.io/) engine. It indexes local repositories, public GitHub repositories, and authenticated GitHub/GitLab repositories, then exposes search through a web UI, JSON endpoints, a packed-context CLI, and an MCP server for coding agents.

This documentation is for people running a Codebeam instance on a laptop, workstation, or internal server.

## What you will set up

A running Codebeam instance has four main pieces:

| Piece | Purpose | Default location |
| --- | --- | --- |
| Go web server | Serves login, repository management, search, code view, JSON API, and MCP/CLI subcommands. | `go run ./cmd/codebeam` or `bin/codebeam` |
| SQLite database | Stores users, OAuth/token identities, repositories, permissions, jobs, and settings. | `.codebeam/codebeam.db` |
| Repository cache | Local clones for remote repositories managed by Codebeam. | `.codebeam/repos/` |
| Zoekt index shards | Search indexes built from selected repositories and branches. | `.codebeam/index/` |

:::caution[Protect the data directory]
The SQLite database holds users, repositories, and settings. Code-host access tokens are encrypted at rest (with a key kept outside the data directory) and Codebeam's own issued tokens are stored only as hashes, but you should still restrict filesystem access to `CODEBEAM_DATA_DIR`, back it up carefully, and never commit it. See [Configuration](/configuration/) for the encryption key.
:::

## Recommended setup path

1. [Get started locally](/getting-started/) with `mise run dev`.
2. [Review configuration](/configuration/) and create a `.env` file.
3. [Configure OAuth or tokens](/oauth/) for GitHub, GitLab, or self-managed GitLab.
4. [Add repositories and indexing rules](/repositories-indexing/).
5. For a shared instance, [deploy the binary behind HTTPS](/deployment/) and disable development login.
6. Connect agents and tools with the [MCP server, JSON API, CLI, or VS Code extension](/integrations/).

## Local vs shared instances

| Mode | Best for | Important settings |
| --- | --- | --- |
| Local development | Trying Codebeam, indexing local code, public GitHub repositories, or one user's private repositories. | Defaults are fine; `CODEBEAM_DEV_LOGIN=true`. |
| Personal always-on server | A private search service on a trusted machine. | Set `CODEBEAM_BASE_URL`, `CODEBEAM_SESSION_SECRET`, and persistent `CODEBEAM_DATA_DIR`. |
| Shared/internal server | A team instance exposed behind a reverse proxy. | Use HTTPS, set `CODEBEAM_DEV_LOGIN=false`, configure OAuth, and protect backups. |

## Important application URLs

| URL | Purpose |
| --- | --- |
| `/login` | Login page with development login and configured OAuth providers. |
| `/sources` | Connect GitHub/GitLab, add public GitHub repos, self-managed GitLab tokens, and local repos. |
| `/repos/manage` | Select repositories, start indexing, manage branches, and watch jobs. |
| `/settings` | Configure remote auto-indexing interval and enablement. |
| `/search` | Search across indexed repositories. |
| `/api/search` and `/api/read` | Authenticated JSON endpoints for agents and scripts. |
