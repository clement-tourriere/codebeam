---
title: Troubleshooting
description: Common setup, OAuth, indexing, search, and deployment problems.
---

## Quick checks

Start with these checks before debugging a specific feature:

```sh
# Is the server reachable?
curl -I http://localhost:8080/login

# Did the generated frontend assets exist?
ls static/app.css static/htmx.min.js

# Can Go tests pass?
go test ./...

# Is Universal Ctags available for symbol indexing?
universal-ctags --version || ctags --version
```

If you run under systemd, watch logs with:

```sh
sudo journalctl -u codebeam -f
```

## Setup and startup

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Page loads without styling | `static/app.css` was not generated or `CODEBEAM_STATIC_DIR` points to the wrong directory. | Run `mise run css:build` or `mise run build`; verify `CODEBEAM_STATIC_DIR`. |
| Server fails to parse templates | `CODEBEAM_TEMPLATE_GLOB` is wrong or shell-expanded too early. | Use an absolute glob such as `CODEBEAM_TEMPLATE_GLOB=/opt/codebeam/templates/*.html`; quote it in interactive shells. |
| `.env` changes are ignored | The binary was run directly, not through mise, or the process was not restarted. | Export variables manually or use an environment file; restart Codebeam. |
| Address already in use | Another process listens on port 8080. | Set `CODEBEAM_ADDR=127.0.0.1:8081` or stop the other process. |
| Cannot create `.codebeam` directories | The process user cannot write to the working directory or data path. | Set `CODEBEAM_DATA_DIR` to a writable path and fix ownership. |

## Login and OAuth

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Development login is visible on a shared instance | `CODEBEAM_DEV_LOGIN` is unset or true. | Set `CODEBEAM_DEV_LOGIN=false` and restart. |
| GitHub/GitLab OAuth button is disabled | Client ID or secret is empty. | Set both env vars and restart. |
| Redirect URI mismatch | Provider callback does not match Codebeam's generated callback. | Register `<CODEBEAM_BASE_URL>/auth/<provider>/callback`. |
| OAuth callback says invalid state | Cookie mismatch, blocked cookies, or callback handled by a different instance. | Allow cookies and make sure the same instance handles start and callback. |
| OAuth works locally but not behind proxy | `CODEBEAM_BASE_URL` still points to `localhost` or HTTP. | Set it to the external HTTPS URL. |
| Private repos are missing after sync | Token/OAuth scopes or user permissions are insufficient. | Check GitHub `repo`/fine-grained read access or GitLab `read_api` + `read_repository`. |

## Repository sync and indexing

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Public GitHub repo cannot be added | URL is not a GitHub repository or the repo is private. | Use `owner/name` or connect GitHub with OAuth/PAT for private repos. |
| Index job fails during clone/fetch | Network, credentials, or repository access problem. | Check the job error, token scopes, and server network access to the code host. |
| Search returns no results for a repo | Repo is not selected, was not indexed, or index failed. | Go to `/repos/manage`, select it, and run indexing. |
| Branch filter returns no results | That branch was not indexed. | Add the branch in Manage repositories and reindex. |
| Local changes are not reflected | Local watcher disabled or repo not selected. | Ensure `CODEBEAM_WATCH_LOCAL_REPOS=true`, repo is selected, and watch logs. |
| Remote repos are stale | Auto-index disabled or interval too long. | Check `/settings` and `CODEBEAM_REMOTE_REFRESH_INTERVAL`. |

## Symbol search

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Symbol-only search returns no matches | Universal Ctags was missing when the repo was indexed. | Install Universal Ctags, set `CODEBEAM_CTAGS_PATH` if needed, restart, and reindex. |
| macOS has `ctags` but symbols still fail | `/usr/bin/ctags` is BSD ctags, not Universal Ctags. | `brew install universal-ctags` and set `CODEBEAM_CTAGS_PATH="$(brew --prefix universal-ctags)/bin/ctags"`. |
| Some languages have fewer symbols | Ctags support varies by language and parser. | Use text search or `find_references` as a fallback. |

## API, MCP, and CLI

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `/api/search` redirects to login | HTTP API requires an authenticated browser session. | Log in first and send session cookies, or use the MCP/CLI tools locally. |
| MCP shows no repositories | MCP process is reading a different data directory. | Set `CODEBEAM_DATA_DIR`, `CODEBEAM_DB_PATH`, or `CODEBEAM_INDEX_DIR` to match the web server. |
| `codebeam ctx` returns weak context | Repositories are not indexed or filters are too narrow. | Reindex and relax `--repo`, `--path`, or `--lang` filters. |

## Deployment

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| OAuth callback uses internal host/port | `CODEBEAM_BASE_URL` is set to the listen address instead of the public URL. | Use `https://codebeam.example.com`. |
| Binary works in repo checkout but fails in `/opt` | Static assets/templates were not copied or env paths are wrong. | Copy `static/` and `templates/`, or set `CODEBEAM_STATIC_DIR` and `CODEBEAM_TEMPLATE_GLOB`. |
| Data disappears after restart in a container | Data directory was not mounted as a persistent volume. | Mount a volume and set `CODEBEAM_DATA_DIR=/data`. |
| Backups are incomplete | Only index shards were backed up. | Back up `codebeam.db` at minimum; preferably the full `CODEBEAM_DATA_DIR`. |

## Reset a local development instance

To start over locally, stop Codebeam and remove the runtime data directory:

```sh
rm -rf .codebeam
mise run dev
```

This deletes users, tokens, repository metadata, clones, and indexes for the local checkout.
