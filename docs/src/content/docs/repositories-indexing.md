---
title: Repositories and indexing
description: Add repositories, choose branches, build indexes, and keep search fresh.
---

Codebeam searches repositories that have been added, selected, and indexed. The repository list and index jobs are managed from `/sources` and `/repos/manage`.

## Supported repository sources

| Source | How to add | Notes |
| --- | --- | --- |
| Local repository | `/sources` → **Add a local repository** | Points at a repository already present on the Codebeam machine. |
| Public GitHub repository | `/sources` → **Add a public GitHub repository** | No OAuth or token required. |
| GitHub OAuth | Configure OAuth, connect from `/sources`, then sync. | Best for shared GitHub login and private repo sync. |
| GitHub PAT | Paste token in `/sources`. | Per-user alternative to OAuth. |
| GitLab.com OAuth | Configure OAuth, connect from `/sources`, then sync. | Uses `CODEBEAM_GITLAB_BASE_URL=https://gitlab.com`. |
| Self-managed GitLab PAT | Enter instance URL and token in `/sources`. | Supports multiple self-managed instances per user. |
| Self-managed GitLab OAuth | Set `CODEBEAM_GITLAB_BASE_URL` and OAuth credentials. | Supports one app-wide GitLab OAuth host. |

## Repository lifecycle

1. **Connect a source** from `/sources`.
2. **Sync repositories** if the source is OAuth or token based.
3. **Select repositories** from `/repos/manage`.
4. **Index** selected repositories.
5. Search on `/search` or through the API/MCP tools.
6. Remove an entire source from `/sources` when you want to delete its saved connection, repository records, index jobs, indexes, and Codebeam-managed clones.

Index jobs move through queued, running, succeeded, failed, or cancelled states. Failed jobs show the last index error in the UI. Removing a local source does not delete the original repositories on disk.

## Branch indexing

Remote repositories can index one, many, or all branches.

- Leave the branch field empty to index the code host's default branch.
- Enter comma-separated branch names or glob patterns to include branches:

```text
main,release/*,feature/search
```

- Use `*` or `all` to index every remote branch.
- Prefix a name or pattern with `!` to exclude it:

```text
*,!wip/*,!dependabot/*
```

On each successful index, Codebeam stores the actual resolved branch names. Deleted remote branches disappear from search after the next refresh because the repo shard is rebuilt from the current branch list. If a branch explicitly named in the policy is missing, the index job fails and the repo is marked as needing reindex rather than silently serving stale data.

### Branch limit

When a pattern like `*` matches more branches than the configured limit (`CODEBEAM_MAX_INDEXED_BRANCHES`, editable on the Settings → Indexing tab), Codebeam indexes only the most recently updated branches instead of cloning and indexing every branch — the default branch is always kept. Ranking uses a metadata-only fetch of branch tips, so it stays fast even on repositories with thousands of branches.

The limit defaults to `20` and can be changed by an admin on the Settings → Indexing tab (or via the environment variable). The search index (Zoekt) stores branch membership in a 64-bit mask, so **at most 64 branches per repository** can be indexed — `0` or values above 64 mean that maximum. If a selection of *explicitly named* branches exceeds the limit, the index job fails with a clear error rather than silently dropping branches you asked for by name; patterns are truncated by recency instead.

### Repositories you can't access

If a remote index run fails with an access or permission error — for example a GitLab project your token can no longer read — Codebeam deselects the repository so the background refresher stops retrying it every cycle, and records why on the repository. Re-select it to retry once access is restored. Turn this off with the "Automatically exclude repositories that fail with an access error" toggle on the Settings → Indexing tab (or `CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE=false`).

Search supports branch filtering through the web UI facets and query controls. The JSON API accepts a `branch` parameter. Equivalent hits across branches are deduplicated in search results and displayed as branch-location badges with a `+N` overflow.

## Local repository freshness

Selected local repositories are watched by default. When files change on disk, Codebeam queues a debounced background reindex.

Disable local watching with:

```dotenv
CODEBEAM_WATCH_LOCAL_REPOS=false
```

Search results for local repositories can show a `dirty` badge when the matching file has uncommitted working-tree changes.

## Remote repository freshness

Selected remote repositories are periodically re-pulled and reindexed by the remote auto-index scheduler. This keeps GitHub, GitLab.com, and self-managed GitLab repositories fresh without webhooks.

Defaults:

```dotenv
CODEBEAM_AUTO_INDEX_REMOTE=true
CODEBEAM_REMOTE_REFRESH_INTERVAL=30m
```

You can also change remote auto-indexing from `/settings`. Environment values are used as defaults until settings are saved.

## Symbol indexing

Symbol search uses Universal Ctags during indexing. Without Universal Ctags, Codebeam still performs normal text search, but symbol-only search will not return definitions.

Install Universal Ctags and point Codebeam at it if auto-detection does not find it.

On macOS with Homebrew:

```sh
brew install universal-ctags
export CODEBEAM_CTAGS_PATH="$(brew --prefix universal-ctags)/bin/ctags"
mise run dev
```

Then reindex repositories so Zoekt shards include symbols.

Codebeam checks `CODEBEAM_CTAGS_PATH` first, then `CTAGS_COMMAND`, then common binaries such as `universal-ctags` and `ctags` on `PATH`. macOS `/usr/bin/ctags` is BSD ctags and is not sufficient.

## Search filters after indexing

The web UI and `/api/search` expose filters for:

- repository (`repo`)
- branch (`branch`)
- path (`path` and top-level path)
- extension and language (`ext`, `lang`)
- source/provider
- dirty or fresh results
- symbol kind
- sort order

Zoekt query syntax is also available for literal and regex search.

## Disk and backup notes

- The SQLite database stores repository metadata, permissions, identities, and tokens.
- Remote clones under `CODEBEAM_REPO_DIR` can often be re-fetched, but keeping them speeds recovery.
- Zoekt shards under `CODEBEAM_INDEX_DIR` can be rebuilt by reindexing, but they may be large.
- For the safest backup, snapshot the full `CODEBEAM_DATA_DIR` while Codebeam is stopped.
