---
title: Repositories and indexing
description: Add repositories from any source, control which branches are indexed, and let Codebeam keep everything fresh.
---

Codebeam searches what it has indexed. This page covers the full lifecycle: adding repositories, choosing branches, monitoring index jobs, and the automatic freshness machinery that means you rarely think about any of it.

## Where repositories come from

Repositories enter Codebeam through the **Sources** page (`/sources`):

| Source | Setup needed | Good for |
| --- | --- | --- |
| Local path | None *(admin)* | Code already on the machine — searched live, including uncommitted changes. |
| Public GitHub repo | None | Any public project — paste `owner/name` or a URL. |
| GitHub personal access token | A token | Your private GitHub repos, without configuring OAuth. |
| GitHub / GitLab OAuth | An OAuth app ([setup](/codebeam/oauth/)) | Shared instances — each user connects their own account. |
| Self-managed GitLab token | Instance URL + token | Company GitLab instances; several can be connected side by side. |

Local paths and public GitHub repositories are indexed as soon as you add them. Connected code hosts work differently: Codebeam **syncs** the list of repositories your account can access, and you then choose which ones to actually index.

Removing a source removes everything it brought in — its repositories, indexes, jobs, and Codebeam-managed clones. Removing a local source never touches the original repository on disk.

## Managing repositories

**Manage repositories** (`/repos/manage`) is the control room. Each repository has an **On/Off** switch: turning it on clones (if remote) and indexes it; turning it off removes its index shards and stops all background refreshing.

Working at scale:

- **Status chips** filter the list to `indexed`, `indexing`, `stale`, `failed`, or `off`, with live counts.
- **Bulk actions** activate or deactivate the checked repositories — or *everything matching the current filter*, so "turn on all Go repositories from GitLab" is two clicks.
- Each source card also has an **activate all / deactivate all** toggle.
- Clicking a repository opens a **detail panel** with its branches, last index result, and per-repo actions (reindex, edit branches, cancel a running job).

Index jobs run in the background and the page polls them live — queued, running, then succeeded, failed, or cancelled. A failed job shows its error message right on the repository.

## Choosing branches

By default, a remote repository indexes its default branch. The branch policy (in the repository's detail panel, or when adding a public repo) accepts a comma-separated list of names and glob patterns:

```text
main, release/*
```

- `*` or `all` indexes every branch.
- `!pattern` excludes branches: `*, !wip/*, !dependabot/*`.

All indexed branches of a repository share one index shard — Zoekt stores identical files once — so indexing several branches is much cheaper than it sounds. Search results then support `branch:` filtering, and equivalent matches across branches collapse into a single result with branch badges.

Local repositories always index the checked-out working tree — whatever branch you're on is what search sees.

:::note[Branch limits]
When a pattern like `*` matches many branches, Codebeam keeps the most recently updated ones up to a limit (default 20, configurable on **Settings → Indexing**; the default branch is always kept). The index format supports at most 64 branches per repository. If you *explicitly name* more branches than the limit, the job fails with a clear error instead of silently dropping some — only patterns are truncated by recency.
:::

If a branch named in the policy disappears from the remote, the index job fails and the repository is marked for reindexing, rather than quietly serving stale data.

## Staying fresh

You should almost never need the reindex button. Two mechanisms keep the index current:

### Local repositories: watched live

Codebeam watches selected local repositories with a filesystem watcher. Save a file, and a debounced reindex runs within seconds — search reflects your editor, not your last commit. Matches from files with uncommitted changes carry a `dirty` badge so you can tell.

Disable with `CODEBEAM_WATCH_LOCAL_REPOS=false` if you'd rather reindex manually.

### Remote repositories: refreshed on a schedule

A background scheduler re-pulls and re-indexes any selected remote repository whose last index is older than the refresh interval (default 30 minutes). It also picks up newly selected repositories, and naturally backs off ones that recently failed. Private repositories fetch with the token of a user who has access; public ones need no token at all.

Tune it on **Settings → Indexing** or via environment variables:

```dotenv
CODEBEAM_AUTO_INDEX_REMOTE=true
CODEBEAM_REMOTE_REFRESH_INTERVAL=30m
```

:::tip[Repositories you lose access to]
If a remote repository fails to index with an access or permission error — say, a GitLab project your token can no longer read — Codebeam deselects it so the scheduler stops retrying every cycle, and records why. Re-select it once access is restored. Disable this behavior on **Settings → Indexing** (`CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE=false`).
:::

## What gets indexed

- Files up to **2 MiB**; larger files are skipped.
- Dependency and build directories are skipped: `.git`, `node_modules`, `target`, `dist`, `build`, `.venv`, `.cache`, and similar.
- If **Universal Ctags** is available, symbols (functions, types, classes…) are extracted during indexing — this is what powers symbol search and the file outline. Codebeam finds ctags automatically on `PATH`; point `CODEBEAM_CTAGS_PATH` at a binary if needed (note: macOS's built-in `/usr/bin/ctags` is BSD ctags and won't work — `brew install universal-ctags`). Repositories indexed *before* ctags was installed need one reindex to gain symbols.

Indexing concurrency adapts to the machine (up to 4 repositories at once, bounded by CPU). See [Configuration](/codebeam/configuration/#indexing-and-freshness) for the knobs.

## Disk and backups

Everything lives under the data directory (default `.codebeam/`):

- `codebeam.db` — the source of truth: users, tokens, repositories, permissions, settings. **Back this up.**
- `repos/` — clones of remote repositories. Re-fetchable, but keeping them speeds recovery.
- `index/` — Zoekt shards. Rebuildable by reindexing; can be large.

The safest backup is a snapshot of the whole data directory while Codebeam is stopped. See [Deployment → Backups](/codebeam/deployment/#backups) for the encryption-key caveat.
