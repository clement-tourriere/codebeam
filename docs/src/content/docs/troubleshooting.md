---
title: Troubleshooting
description: Common setup, sign-in, indexing, search, and deployment problems — and their fixes.
---

## Quick checks

Before debugging a specific feature:

```sh
# Which version is running, and is it the right binary?
codebeam version
which codebeam

# Is the server reachable?
curl -I http://localhost:8080/login

# Is git available? (required)
git --version

# Is Universal Ctags available? (needed for symbol search)
universal-ctags --version || ctags --version
```

Under systemd, watch logs with `sudo journalctl -u codebeam -f`; with Docker, `docker logs -f codebeam`.

## Setup and startup

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Address already in use | Another process listens on port 8080. | Set `CODEBEAM_ADDR=127.0.0.1:8081` or stop the other process. |
| Cannot create `.codebeam` directories | The process user can't write to the working directory. | Set `CODEBEAM_DATA_DIR` to a writable path and fix ownership. |
| `.env` changes are ignored | The process wasn't restarted, or the binary was run outside mise. | Restart Codebeam; when not using mise, export the variables or use an environment file. |
| Page loads without styling (source checkout) | Frontend assets not built, or `CODEBEAM_STATIC_DIR` wrong. | Run `mise run dev` (or `mise run build`); check `CODEBEAM_STATIC_DIR`. |
| Server fails to parse templates (source checkout) | `CODEBEAM_TEMPLATE_GLOB` wrong or shell-expanded. | Use an absolute, quoted glob such as `'/opt/codebeam/templates/*.html'`. |

## Sign-in and OAuth

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Development login shows on a shared instance | `CODEBEAM_DEV_LOGIN` explicitly set to true. | Set `CODEBEAM_DEV_LOGIN=false` and restart (it is off by default for non-localhost base URLs). |
| OAuth/SSO button missing or disabled | Client ID or secret empty. | Set both variables and restart. |
| Redirect URI mismatch | Provider callback ≠ Codebeam's generated callback. | Register exactly `<CODEBEAM_BASE_URL>/auth/<provider>/callback`. |
| Invalid OAuth state on callback | Blocked cookies, or another instance handled the callback. | Allow cookies; make sure one instance handles start and callback. |
| Works locally, fails behind a proxy | `CODEBEAM_BASE_URL` still `localhost` or HTTP. | Set it to the external HTTPS URL. |
| Private repos missing after sync | Insufficient token/OAuth scopes, or no access. | GitHub: `repo` scope (or fine-grained contents+metadata read). GitLab: `read_api` + `read_repository`. |

## Repositories and indexing

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Public GitHub repo can't be added | Not a GitHub URL, or the repo is private. | Use `owner/name`; connect GitHub OAuth/PAT for private repos. |
| Index job fails during clone/fetch | Network, credentials, or access problem. | Read the job's error in the repo panel; check token scopes and network reach. |
| A repo returns no search results | Not selected, not yet indexed, or the job failed. | On `/repos/manage`, switch it On and check its status. |
| Branch filter finds nothing | That branch isn't in the repo's branch policy. | Edit branches in the repo panel and reindex. |
| Local edits not showing in search | Watcher disabled, or the repo isn't selected. | Ensure `CODEBEAM_WATCH_LOCAL_REPOS=true` and the repo is On. |
| Remote repos go stale | Auto-indexing disabled or interval too long. | Check **Settings → Indexing** and `CODEBEAM_REMOTE_REFRESH_INTERVAL`. |
| A repo keeps getting deselected | Auto-exclude after repeated access failures. | Fix the token/access, then re-select it; or disable auto-exclude on **Settings → Indexing**. |

## Search

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Symbols-only search returns nothing | Universal Ctags missing when the repo was indexed. | Install Universal Ctags, set `CODEBEAM_CTAGS_PATH` if not auto-detected, restart, **reindex**. |
| macOS has `ctags` but symbols still fail | `/usr/bin/ctags` is BSD ctags, not Universal. | `brew install universal-ctags`; Codebeam rejects BSD ctags on purpose. |
| Some languages have few symbols | Ctags parser coverage varies per language. | Fall back to text search or find-references. |
| Structural search rejects the language | Only 11 languages are bundled. | Use one of: bash, c, go, java, javascript, json, python, rust, tsx, typescript, yaml. |
| Structural results flagged truncated | Search hit the time/file/match caps. | Narrow with repo/path filters, or raise the [structural limits](/codebeam/configuration/#structural-ast-search). |
| Query behaves unexpectedly | Filter atoms or regex interpreted differently than intended. | Check the compiled-query badge under the search box; see [Searching](/codebeam/searching/). |

## API, MCP, and CLI

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `/api/*` or `/mcp` returns 401 | No valid credential on the request. | Send `Authorization: Bearer cbp_…` ([API token](/codebeam/oauth/#codebeam-api-tokens)), or let the MCP client run the OAuth flow. |
| Token stopped working | Expired or revoked. | Check **Settings → API tokens** / **Connected agents**; create a new one. |
| MCP (stdio) or `ctx` sees no repositories | The process reads a different data directory than the server. | Set the same `CODEBEAM_DATA_DIR` (or `CODEBEAM_DB_PATH`/`CODEBEAM_INDEX_DIR`) for the CLI/MCP invocation. |
| `codebeam ctx` returns thin context | Repos not indexed, or filters too narrow. | Reindex; relax `--repo`, `--path`, `--lang`; raise `--max-files`/`--max-chars`. |

## Deployment

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| OAuth callback uses an internal host/port | `CODEBEAM_BASE_URL` set to the listen address. | Set it to the public URL, e.g. `https://codebeam.example.com`. |
| Data gone after a container restart | No persistent volume. | Mount volumes for `/data` **and** `/config`. |
| Code-host connections broken after restart (container) | The auto-generated encryption key wasn't persisted, so a new key was created. | Persist `/config`, or set `CODEBEAM_ENCRYPTION_KEY` explicitly; users must reconnect their code hosts once. |
| Backups incomplete | Only index shards were saved. | Back up `codebeam.db` at minimum, ideally all of `CODEBEAM_DATA_DIR` — and the encryption key separately. |

## Reset a local instance

To start over, stop Codebeam and delete the data directory:

```sh
rm -rf .codebeam
codebeam
```

This deletes users, tokens, repository metadata, clones, and indexes for that instance. Local source repositories on disk are untouched.
