---
title: Configuration
description: Environment variables, data paths, secrets, and runtime settings for Codebeam.
---

Codebeam reads configuration from environment variables at process start. When you run through mise, `mise.toml` loads `.env` automatically for `mise run dev`, `mise run build`, and other mise tasks.

If you run the binary directly, load the environment yourself:

```sh
set -a
. /etc/codebeam/codebeam.env
set +a
/opt/codebeam/bin/codebeam
```

## Minimal local `.env`

```dotenv
CODEBEAM_BASE_URL=http://localhost:8080
```

Development login is enabled by default, so OAuth is optional for local testing.

## Recommended shared-instance `.env`

```dotenv
CODEBEAM_ADDR=127.0.0.1:8080
CODEBEAM_BASE_URL=https://codebeam.example.com
CODEBEAM_DATA_DIR=/var/lib/codebeam
CODEBEAM_SESSION_SECRET=replace-with-a-long-random-value
CODEBEAM_DEV_LOGIN=false
# Optional: a key is auto-generated on first run if unset. Set it explicitly in
# containers (or mount CODEBEAM_ENCRYPTION_KEY_FILE on a persistent volume).
CODEBEAM_ENCRYPTION_KEY=replace-with-a-long-random-value

CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...

# Optional GitLab.com or self-managed GitLab OAuth.
CODEBEAM_GITLAB_BASE_URL=https://gitlab.com
CODEBEAM_GITLAB_CLIENT_ID=...
CODEBEAM_GITLAB_CLIENT_SECRET=...
```

Generate the session secret and encryption key with a command such as:

```sh
openssl rand -base64 32
```

## Environment variable reference

### Server and authentication

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_ADDR` | `:8080` | HTTP listen address. Use `127.0.0.1:8080` behind a local reverse proxy or `:8080` inside a container. |
| `CODEBEAM_BASE_URL` | `http://localhost:8080` | Public URL users open in the browser. OAuth callback URLs are built from this value. Do not include a trailing slash. |
| `CODEBEAM_SESSION_SECRET` | `dev-secret-change-me` | Secret used to sign session and OAuth state cookies. Change this before any shared deployment. |
| `CODEBEAM_DEV_LOGIN` | loopback only | Enables the passwordless **Continue in development mode** login. Defaults on only when `CODEBEAM_BASE_URL` is loopback (localhost/127.0.0.1/::1) and off for any real host, so a reachable instance never ships an unauthenticated admin door. The button is POST-only. Set explicitly to override. |
| `CODEBEAM_ENCRYPTION_KEY` | empty | Master key for AES-256-GCM encryption of code-host access tokens at rest (`identities.access_token`). Any non-empty string works; generate one with `openssl rand -base64 32`. When empty, the key is read from (or generated into) `CODEBEAM_ENCRYPTION_KEY_FILE`. |
| `CODEBEAM_ENCRYPTION_KEY_FILE` | `<user config dir>/codebeam/encryption.key` | Path to a file holding the encryption key. When neither this nor `CODEBEAM_ENCRYPTION_KEY` is set, a random key is auto-generated here (`0600`) on first run and reused on restart — so encryption is always on with no setup. Keep it **outside** `CODEBEAM_DATA_DIR` so a stolen data backup does not also carry the key. In containers, point this at a persistent volume or set `CODEBEAM_ENCRYPTION_KEY`, or a new key is generated each restart and existing tokens become unreadable. |

### Data and asset paths

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_DATA_DIR` | `.codebeam` | Base directory for Codebeam runtime data. |
| `CODEBEAM_DB_PATH` | `$CODEBEAM_DATA_DIR/codebeam.db` | SQLite database path. Contains users, identities, repositories, jobs, and settings. Code-host access tokens are encrypted at rest (see `CODEBEAM_ENCRYPTION_KEY`); Codebeam's own issued tokens are stored only as hashes. |
| `CODEBEAM_REPO_DIR` | `$CODEBEAM_DATA_DIR/repos` | Directory for Codebeam-managed remote clones. |
| `CODEBEAM_INDEX_DIR` | `$CODEBEAM_DATA_DIR/index` | Directory for Zoekt search index shards. |
| `CODEBEAM_STATIC_DIR` | `static` | Directory served at `/static/`. Must contain generated `app.css` and `htmx.min.js`. |
| `CODEBEAM_TEMPLATE_GLOB` | `templates/*.html` | Glob used to load HTML templates. Quote this value in shell files if it contains `*`. |

### GitHub and GitLab

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_GITHUB_CLIENT_ID` | empty | GitHub OAuth App client ID. Enables GitHub OAuth login/connect when paired with the secret. |
| `CODEBEAM_GITHUB_CLIENT_SECRET` | empty | GitHub OAuth App client secret. |
| `CODEBEAM_GITLAB_BASE_URL` | `https://gitlab.com` | GitLab OAuth host. Set to your self-managed GitLab URL for self-managed OAuth. |
| `CODEBEAM_GITLAB_CLIENT_ID` | empty | GitLab OAuth application ID. Enables GitLab OAuth login/connect when paired with the secret. |
| `CODEBEAM_GITLAB_CLIENT_SECRET` | empty | GitLab OAuth application secret. |
| `CODEBEAM_OIDC_ISSUER` | empty | OpenID Connect issuer URL for SSO login (Okta, Entra ID, Google Workspace, Keycloak, …). Setting issuer + client id + secret enables the SSO button. |
| `CODEBEAM_OIDC_CLIENT_ID` | empty | OIDC client id. |
| `CODEBEAM_OIDC_CLIENT_SECRET` | empty | OIDC client secret. |
| `CODEBEAM_OIDC_NAME` | `SSO` | Label shown on the login button ("Continue with …"). |
| `CODEBEAM_OIDC_SCOPES` | `openid profile email` | Scopes requested from the provider. |
| `CODEBEAM_OIDC_ALLOWED_DOMAINS` | empty | Comma-separated email domains allowed to sign in via SSO; empty allows any. |

Personal access tokens entered in the UI do not require these app-wide OAuth variables.

### Indexing and freshness

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_WATCH_LOCAL_REPOS` | `true` | Watches selected local repositories and queues debounced reindexes when files change. |
| `CODEBEAM_AUTO_INDEX_REMOTE` | `true` | Default setting for the remote auto-index scheduler. The `/settings` page can persist an override. |
| `CODEBEAM_REMOTE_REFRESH_INTERVAL` | `30m` | Default age after which selected remote repositories are re-pulled and reindexed. Uses Go duration syntax such as `15m`, `1h`, or `24h`. |
| `CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE` | `true` | When a remote repository fails to index with an access/permission error (e.g. a GitLab project the token can't read), deselect it so the scheduler stops retrying it. Re-select it to retry once access is restored. The `/settings` page can persist an override. |
| `CODEBEAM_MAX_INDEXED_BRANCHES` | `20` | Cap on how many branches a single repository indexes when a branch pattern (such as `*`) matches many. Only the most recently updated branches are kept, and the default branch always is. The search index supports at most 64 branches per repository, so `0` and values above `64` mean that maximum. The `/settings` page can persist an override. |
| `CODEBEAM_SYNC_PERMISSIONS` | `true` | Periodically mirror each user's GitHub/GitLab repository access into Codebeam's per-user permissions, so lost or gained access to a private repo propagates to search without a manual re-sync. Public repos are never pruned. |
| `CODEBEAM_PERMISSION_SYNC_INTERVAL` | `1h` | How often the permission sync runs. Go duration syntax. |
| `CODEBEAM_INDEX_CONCURRENCY` | CPU-bounded, max 4 | Maximum repository indexing jobs that can run at the same time. |
| `CODEBEAM_INDEX_FILE_CONCURRENCY` | CPU-bounded, max 8 | Per-repository parallelism for reading filesystem files and Git blobs while building a shard. |
| `CODEBEAM_CTAGS_PATH` | empty | Explicit path to a Universal Ctags binary for symbol indexing. |
| `CTAGS_COMMAND` | empty | Zoekt-compatible fallback used when `CODEBEAM_CTAGS_PATH` is not set. |

When no Universal Ctags binary is available, Codebeam still indexes and searches text, but symbol search returns no symbol matches until you install ctags and reindex repositories.

### Structural (AST) search

Structural search runs an embedded ast-grep engine in-process (a WebAssembly module bundled in the binary — nothing to install). These variables bound how much work a single structural search may do:

| Variable | Default | Description |
| --- | --- | --- |
| `CODEBEAM_STRUCTURAL_TIMEOUT` | `15s` | Maximum wall-clock time for one structural search. Hitting it returns partial results flagged as truncated. Go duration syntax. |
| `CODEBEAM_STRUCTURAL_MAX_FILES` | `5000` | Maximum number of candidate files scanned per search. |
| `CODEBEAM_STRUCTURAL_MAX_MATCHES` | `1000` | Maximum number of matches returned per search. |

## Boolean and duration values

Boolean variables are enabled by `1`, `true`, `yes`, or `on` (case-insensitive). Use values such as `false`, `0`, `no`, or `off` to disable a feature.

Duration variables use Go duration syntax:

```dotenv
CODEBEAM_REMOTE_REFRESH_INTERVAL=15m
CODEBEAM_REMOTE_REFRESH_INTERVAL=1h
CODEBEAM_REMOTE_REFRESH_INTERVAL=24h
```

## When changes take effect

Most environment variables are read once at startup. Restart Codebeam after editing `.env` or an environment file.

Remote auto-index settings are special: Codebeam starts the scheduler on boot, then reads the live setting from SQLite each poll. Values from environment variables are defaults until you save settings on `/settings`.

## Security checklist

Before exposing Codebeam outside your laptop:

- Set `CODEBEAM_SESSION_SECRET` to a random value.
- Set `CODEBEAM_DEV_LOGIN=false`.
- Serve through HTTPS, usually with a reverse proxy.
- Restrict filesystem permissions on `CODEBEAM_DATA_DIR`.
- Keep the encryption key outside `CODEBEAM_DATA_DIR` (the default location already is) so a stolen data backup can't decrypt the stored code-host tokens — and back the key up separately, because losing it makes those tokens unrecoverable (affected users must reconnect their code host).
- Back up `CODEBEAM_DATA_DIR` or at least `codebeam.db`.
- Keep OAuth callback URLs aligned with `CODEBEAM_BASE_URL`.
