---
title: Getting started
description: Install Codebeam, sign in, add repositories, or run it from source.
---

## Install a release

The fastest path is a released binary — it is fully self-contained (web UI assets embedded):

```sh
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
codebeam
```

The script detects your OS/architecture (Linux and macOS, amd64/arm64), verifies the SHA-256 checksum, and installs to `/usr/local/bin` (or `~/.local/bin` when that is not writable; override with `CODEBEAM_INSTALL_DIR`, pin a version with `CODEBEAM_VERSION=v0.2.0`). Archives are also on the [releases page](https://github.com/clement-tourriere/codebeam/releases).

Alternatively, run the Docker image — see [Deployment](/codebeam/deployment/):

```sh
docker run -d --name codebeam -p 8080:8080 \
  -v codebeam-data:/data -v codebeam-config:/config \
  ghcr.io/clement-tourriere/codebeam:latest
```

### Runtime requirements

- **`git` on `PATH` is required** — Codebeam shells out to it to clone, fetch, and read repositories. The Docker image ships it; for the binary install, install git through your package manager first.
- Optional: [Universal Ctags](https://github.com/universal-ctags/ctags) enables symbol search.

The rest of this page covers running from source; skip to [step 2](#2-create-your-environment-file) if you installed a release.

## Run from source

Codebeam is built with Go and a small Node-based asset pipeline.

- [mise](https://mise.jdx.dev/) to install the pinned toolchain and run tasks.
- Go `1.25.11` and Node `22` if you do not use mise.

## 1. Clone and install

```sh
git clone https://github.com/clement-tourriere/codebeam.git
cd codebeam
mise install
```

`mise install` reads `mise.toml` and installs the Go, Node, `hk`, `pkl`, and `commitizen` versions used by this repository.

## 2. Create your environment file

```sh
cp .env.example .env
$EDITOR .env
```

For a first local run, this is enough:

```dotenv
CODEBEAM_BASE_URL=http://localhost:8080
```

OAuth settings can stay empty until you want to connect private GitHub/GitLab repositories through OAuth. You can still add public GitHub repositories or sign in locally.

## 3. Start the web server

```sh
mise run dev
```

This task:

1. installs frontend npm dependencies if missing,
2. builds Tailwind/DaisyUI CSS and copies HTMX into `static/`, and
3. runs `go run ./cmd/codebeam`.

Open <http://localhost:8080>.

## 4. Sign in for the first time

By default, local development login is enabled. On `/login`, click **Continue in development mode**.

For a shared instance, disable development login and use OAuth instead:

```dotenv
CODEBEAM_DEV_LOGIN=false
CODEBEAM_SESSION_SECRET=replace-with-a-long-random-secret
```

Code-host access tokens are encrypted at rest automatically. If you don't set `CODEBEAM_ENCRYPTION_KEY`, Codebeam generates a key on first run, stores it outside the data directory (under your user config dir), and reuses it on restart; in containers, set the key explicitly so it survives restarts.

See [Configuration](/codebeam/configuration/) and [OAuth and tokens](/codebeam/oauth/) before exposing Codebeam to other users.

## 5. Add and index repositories

Go to `/sources` and choose one of these paths:

- **Public GitHub repository**: paste `owner/name` or a GitHub URL. No OAuth is required.
- **GitHub token**: paste a personal access token to sync private or internal repositories without app-wide OAuth variables.
- **GitHub/GitLab OAuth**: configure the provider in `.env`, restart Codebeam, then connect from `/sources`.
- **Self-managed GitLab token**: enter the instance URL and a token with `read_api` and `read_repository`.
- **Local repository**: add an absolute or relative path to a repository already on the machine.

Then visit `/repos/manage`, select repositories, and start indexing. Search results appear on `/search` after indexes are built.

## Build a binary

```sh
mise run build
```

The binary is written to `bin/codebeam`. It embeds the web UI assets that exist at build time (the task builds the CSS first), so it can run from any directory. When the working directory contains `static/` and `templates/` — such as the repository root — those on-disk copies take precedence, which is what makes `mise run dev` pick up template edits. To point at assets elsewhere:

```sh
CODEBEAM_STATIC_DIR=/opt/codebeam/static \
CODEBEAM_TEMPLATE_GLOB='/opt/codebeam/templates/*.html' \
/opt/codebeam/bin/codebeam
```

## Without mise

If you do not use mise, install Go and Node manually, then run:

```sh
npm --prefix frontend install
npm --prefix frontend run build
go run ./cmd/codebeam
```

For tests:

```sh
go test ./...
```

## Data created on first run

By default, Codebeam creates `.codebeam/` in the working directory:

```text
.codebeam/
├── codebeam.db    # SQLite metadata: users, identities, repos, jobs, settings (code-host tokens encrypted at rest)
├── index/         # Zoekt shards
└── repos/         # managed remote clones
```

Move this directory with `CODEBEAM_DATA_DIR`, or override individual paths with `CODEBEAM_DB_PATH`, `CODEBEAM_REPO_DIR`, and `CODEBEAM_INDEX_DIR`.

The token-encryption key is deliberately **not** stored here. Unless you set `CODEBEAM_ENCRYPTION_KEY` or `CODEBEAM_ENCRYPTION_KEY_FILE`, it is written to `<user config dir>/codebeam/encryption.key` on first run — outside the data directory, so backing up `.codebeam/` never captures the key.
