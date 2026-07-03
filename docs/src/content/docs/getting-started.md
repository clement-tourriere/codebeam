---
title: Get started
description: Install Codebeam, add your first repository, and run your first search in five minutes.
---

This page takes you from nothing to your first search result. You need a machine with `git` installed — that's the only hard requirement.

## 1. Install

**One-line install** (Linux and macOS, amd64/arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
```

The script downloads the release for your platform, verifies its SHA-256 checksum, and installs to `/usr/local/bin` (falling back to `~/.local/bin` when that isn't writable). The binary is fully self-contained — the web UI is embedded, so there is nothing else to download.

**Docker**, if you prefer containers:

```sh
docker run -d --name codebeam -p 8080:8080 \
  -v codebeam-data:/data -v codebeam-config:/config \
  ghcr.io/clement-tourriere/codebeam:latest
```

You can also grab an archive from the [releases page](https://github.com/clement-tourriere/codebeam/releases), or [build from source](#run-from-source).

:::tip[Optional: symbol search]
Install [Universal Ctags](https://github.com/universal-ctags/ctags) (`brew install universal-ctags` on macOS) to enable symbol search — finding *definitions* rather than every mention of a name. Codebeam detects it automatically; everything else works without it. The Docker image already includes it.
:::

## 2. Start Codebeam

```sh
codebeam
```

Open <http://localhost:8080>. On the login page, click **Continue in development mode** — no password needed. This passwordless login is enabled automatically when Codebeam runs on localhost, and disabled automatically the moment you configure a real host, so a shared instance never ships an open door. The first user to sign in becomes the instance admin.

Codebeam creates a `.codebeam/` directory in the working directory for its database, clones, and indexes. Set `CODEBEAM_DATA_DIR` to put it somewhere else — see [Configuration](/codebeam/configuration/).

## 3. Add a repository

Go to **Sources** (`/sources`). The two zero-setup options:

- **A local repository** — enter the path to any git repository already on the machine, e.g. `~/work/my-app`. *(Admin only on shared instances, since it reads the server's disk.)*
- **A public GitHub repository** — paste `owner/name` or a GitHub URL, e.g. `sourcegraph/zoekt`. No account or token needed.

To index your **private repositories**, connect a code host: paste a GitHub personal access token directly on the Sources page, or set up [GitHub/GitLab OAuth or SSO](/codebeam/oauth/) for a shared instance.

Public repositories and local paths are selected and indexed immediately when you add them. Repositories synced from a connected code host land on **Manage repositories** (`/repos/manage`), where you flip the ones you want to **On** — each one is cloned and indexed in the background, and you can watch the jobs live on the same page.

## 4. Search

Head to **Search** (`/search`) and type a query:

```text
parseConfig
```

Results appear with syntax highlighting, and you can narrow them with the filter sidebar (repository, language, path, branch…). Try a regex — queries are regex by default:

```text
func (\w+) Handler
```

Or flip on **Symbols only** to jump straight to a definition. Click any result to open the file in the code viewer, complete with a symbol outline and find-references links.

That's the core loop. Two things now happen automatically:

- **Local repositories stay live.** Codebeam watches them and re-indexes within seconds of a file save — search finds code you haven't committed yet, badged as `dirty`.
- **Remote repositories stay fresh.** A scheduler re-pulls and re-indexes them every 30 minutes (configurable).

## Where to next?

- [Searching](/codebeam/searching/) — query syntax, symbol and structural search, filters, and the code viewer.
- [Repositories and indexing](/codebeam/repositories-indexing/) — branch policies, freshness, and managing many repositories.
- [AI agents and APIs](/codebeam/integrations/) — give Claude Code (or any MCP client) access to your index:

  ```sh
  claude mcp add codebeam -- codebeam mcp
  ```

- [Deployment](/codebeam/deployment/) — run Codebeam as a shared service for your team.

---

## Run from source

Only needed if you want to hack on Codebeam itself. The repository uses [mise](https://mise.jdx.dev/) to pin the toolchain (Go, Node, and dev tools):

```sh
git clone https://github.com/clement-tourriere/codebeam.git
cd codebeam
mise install
mise run dev
```

`mise run dev` installs frontend npm dependencies if missing, builds the Tailwind/DaisyUI CSS, and runs `go run ./cmd/codebeam`. Copy `.env.example` to `.env` for local configuration — mise loads it automatically.

Without mise, install Go and Node yourself, then:

```sh
npm --prefix frontend install
npm --prefix frontend run build
go run ./cmd/codebeam
```

`mise run build` produces a self-contained binary at `bin/codebeam` with the web assets embedded. When the working directory contains `static/` and `templates/` (such as the repository root), those on-disk copies take precedence over the embedded ones — which is what makes `mise run dev` pick up template edits. Point at assets elsewhere with `CODEBEAM_STATIC_DIR` and `CODEBEAM_TEMPLATE_GLOB`.

Run tests with `go test ./...`.

## Data created on first run

```text
.codebeam/
├── codebeam.db    # SQLite: users, identities, repositories, permissions, jobs, settings
├── index/         # Zoekt search index shards
└── repos/         # clones of remote repositories
```

Relocate everything with `CODEBEAM_DATA_DIR`, or override individual paths with `CODEBEAM_DB_PATH`, `CODEBEAM_REPO_DIR`, and `CODEBEAM_INDEX_DIR`.

One thing deliberately does **not** live here: the key used to encrypt code-host tokens at rest. It is auto-generated into `<user config dir>/codebeam/encryption.key` on first run — outside the data directory, so a backup of `.codebeam/` never carries the key with it. See [Configuration](/codebeam/configuration/#server-and-authentication) for the details.
