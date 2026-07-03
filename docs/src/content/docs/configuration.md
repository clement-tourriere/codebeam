---
title: Configuration
description: Complete environment-variable reference — server, secrets, paths, code hosts, indexing, and search limits.
---

Codebeam is configured entirely through environment variables, read once at process start. **Defaults are designed so that a local instance needs no configuration at all** — you only set variables when you move data, share the instance, or connect code hosts.

Running from a source checkout with mise, `.env` in the repository root is loaded automatically. Running the binary directly, load an environment file yourself:

```sh
set -a
. /etc/codebeam/codebeam.env
set +a
codebeam
```

## Typical configurations

**Local instance** — nothing required. Optionally pin the data location:

```dotenv
CODEBEAM_DATA_DIR=/Users/me/.codebeam
```

**Shared instance** — the settings that matter before anyone else connects:

```dotenv
CODEBEAM_ADDR=127.0.0.1:8080
CODEBEAM_BASE_URL=https://codebeam.example.com
CODEBEAM_DATA_DIR=/var/lib/codebeam
CODEBEAM_SESSION_SECRET=replace-with-a-long-random-value
CODEBEAM_ENCRYPTION_KEY=replace-with-a-long-random-value

CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...
```

Generate secrets with `openssl rand -base64 32`. See the [deployment guide](/codebeam/deployment/) for the full production checklist.

## Server and authentication

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_ADDR` | `:8080` | HTTP listen address. Use `127.0.0.1:8080` behind a local reverse proxy. |
| `CODEBEAM_BASE_URL` | `http://localhost:8080` | The public URL users open. OAuth callbacks are built from it — it must match exactly, with no trailing slash. |
| `CODEBEAM_SESSION_SECRET` | `dev-secret-change-me` | Signs session and OAuth-state cookies. Change before any shared deployment. |
| `CODEBEAM_DEV_LOGIN` | auto | Passwordless **Continue in development mode** login. Auto-enabled only when `CODEBEAM_BASE_URL` is loopback, auto-disabled for any real host. Set explicitly to override. |
| `CODEBEAM_ENCRYPTION_KEY` | auto | Master key for AES-256-GCM encryption of stored code-host tokens. When empty, a key is auto-generated (see next row). |
| `CODEBEAM_ENCRYPTION_KEY_FILE` | `<user config dir>/codebeam/encryption.key` | File holding the encryption key. Auto-generated (`0600`) on first run when no key is set — so encryption is always on with zero setup. Kept deliberately **outside** the data directory so a stolen data backup can't decrypt the tokens. In containers, persist this path or set `CODEBEAM_ENCRYPTION_KEY`, otherwise a fresh key each restart makes existing tokens unreadable. |

### Sign-in providers

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_GITHUB_CLIENT_ID` / `_SECRET` | empty | GitHub OAuth App credentials. Both set → GitHub login and repo sync are enabled. |
| `CODEBEAM_GITLAB_BASE_URL` | `https://gitlab.com` | The GitLab host used for OAuth — set to your self-managed URL if applicable (one OAuth host per instance). |
| `CODEBEAM_GITLAB_CLIENT_ID` / `_SECRET` | empty | GitLab OAuth application credentials. |
| `CODEBEAM_OIDC_ISSUER` | empty | OIDC issuer URL for SSO (Okta, Entra ID, Google Workspace, Keycloak, …). Issuer + client ID + secret enable the SSO button. |
| `CODEBEAM_OIDC_CLIENT_ID` / `_SECRET` | empty | OIDC client credentials. |
| `CODEBEAM_OIDC_NAME` | `SSO` | Label on the login button ("Continue with …"). |
| `CODEBEAM_OIDC_SCOPES` | `openid profile email` | Scopes requested from the provider. |
| `CODEBEAM_OIDC_ALLOWED_DOMAINS` | empty | Comma-separated email domains allowed to sign in; empty allows any. |

Personal access tokens entered in the UI need none of these — they are per-user and stored in the database.

## Data and asset paths

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_DATA_DIR` | `.codebeam` | Base directory for all runtime data. |
| `CODEBEAM_DB_PATH` | `$CODEBEAM_DATA_DIR/codebeam.db` | SQLite database — users, identities, repositories, permissions, jobs, settings. |
| `CODEBEAM_REPO_DIR` | `$CODEBEAM_DATA_DIR/repos` | Clones of remote repositories. |
| `CODEBEAM_INDEX_DIR` | `$CODEBEAM_DATA_DIR/index` | Zoekt index shards. |
| `CODEBEAM_STATIC_DIR` | `static` | On-disk override for the embedded web assets (development). |
| `CODEBEAM_TEMPLATE_GLOB` | `templates/*.html` | On-disk override for the embedded HTML templates (development). Quote it in shell files. |

## Indexing and freshness

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_WATCH_LOCAL_REPOS` | `true` | Watch selected local repositories and reindex (debounced) when files change. |
| `CODEBEAM_AUTO_INDEX_REMOTE` | `true` | Background scheduler that re-pulls and reindexes selected remote repositories. |
| `CODEBEAM_REMOTE_REFRESH_INTERVAL` | `30m` | How stale a remote repository may get before refresh. Go durations (`15m`, `1h`, `24h`). |
| `CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE` | `true` | Deselect a remote repository after an access/permission failure so the scheduler stops retrying; re-select to retry. |
| `CODEBEAM_MAX_INDEXED_BRANCHES` | `20` | Branch cap when a pattern matches many; most recently updated branches are kept (default branch always). Hard engine maximum is 64 — `0` or higher values mean 64. |
| `CODEBEAM_SYNC_PERMISSIONS` | `true` | Periodically mirror each user's code-host repository access into Codebeam permissions. |
| `CODEBEAM_PERMISSION_SYNC_INTERVAL` | `1h` | Permission-sync cadence. |
| `CODEBEAM_INDEX_CONCURRENCY` | CPU-bounded, ≤4 | Repository index jobs running at once. |
| `CODEBEAM_INDEX_FILE_CONCURRENCY` | CPU-bounded, ≤8 | Per-repository file/blob read parallelism. |
| `CODEBEAM_CTAGS_PATH` | auto-detect | Explicit path to a Universal Ctags binary for symbol indexing. |
| `CTAGS_COMMAND` | empty | Zoekt-compatible fallback when `CODEBEAM_CTAGS_PATH` is unset. |

`CODEBEAM_AUTO_INDEX_REMOTE`, `CODEBEAM_REMOTE_REFRESH_INTERVAL`, `CODEBEAM_MAX_INDEXED_BRANCHES`, and `CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE` act as *defaults*: once an admin saves values on **Settings → Indexing**, the saved values win.

Without Universal Ctags, text and structural search work normally; only symbol search comes back empty until ctags is installed and repositories are reindexed.

## Structural (AST) search

Structural search runs an embedded ast-grep engine in-process (a WebAssembly module bundled in the binary — nothing to install). These bound one search:

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_STRUCTURAL_TIMEOUT` | `15s` | Wall-clock cap per search; hitting it returns partial results flagged `truncated`. |
| `CODEBEAM_STRUCTURAL_MAX_FILES` | `5000` | Candidate files scanned per search. |
| `CODEBEAM_STRUCTURAL_MAX_MATCHES` | `1000` | Matches returned per search. |

## Value formats and reloading

- **Booleans**: `1`/`true`/`yes`/`on` enable, `0`/`false`/`no`/`off` disable (case-insensitive).
- **Durations**: Go syntax — `90s`, `15m`, `1h`, `24h`.
- **Reloading**: variables are read once at startup — restart after editing. The exception is the indexing settings listed above, which are re-read live from the database once saved on `/settings`.

## Security checklist

Before exposing Codebeam beyond your laptop:

- Set a random `CODEBEAM_SESSION_SECRET`.
- Set `CODEBEAM_DEV_LOGIN=false` (it is already off for non-loopback base URLs, but be explicit).
- Serve HTTPS through a reverse proxy, and set `CODEBEAM_BASE_URL` to the external URL.
- Restrict filesystem permissions on the data directory and environment file.
- Keep the encryption key **outside** the data directory (the default already is) and back it up separately — losing it makes stored code-host tokens unrecoverable, and storing it with the data backup defeats the encryption.
- Back up `CODEBEAM_DATA_DIR`, or at minimum `codebeam.db`.
