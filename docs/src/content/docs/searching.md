---
title: Searching
description: Query syntax, symbol and structural search, result filters, and the code viewer.
---

Everything on this page happens on the **Search** page (`/search`). The same capabilities are available to scripts and agents through the [JSON API and MCP tools](/codebeam/integrations/).

## Writing queries

Queries are **regular expressions by default**, matched against file content and file names:

| You type | You find |
| --- | --- |
| `parseConfig` | The string `parseConfig` anywhere. |
| `func (\w+) Handler` | Regex match — capture groups and character classes work. |
| `"exact phrase here"` | A quoted literal, spaces included. |
| `error -test` | Files matching `error`, excluding matches for `test`. |
| `readFile or writeFile` | Either term (`or` combines expressions). |

Search is case-insensitive unless your query contains an uppercase letter (smart case). Force it either way with `case:yes` or `case:no`.

### Filter atoms

You can scope a query inline with Zoekt filter atoms, exactly like the search field of a code host:

| Atom | Example | Effect |
| --- | --- | --- |
| `repo:` | `repo:codebeam` | Only repositories whose name matches. |
| `file:` | `file:\.go$` | Only paths matching the regex. |
| `lang:` | `lang:python` | Only files in that language. |
| `branch:` | `branch:release/2.x` | Only matches on that indexed branch. |
| `sym:` | `sym:ParseQuery` | Only symbol definitions (see below). |
| `case:` | `case:yes` | Force case-sensitive or insensitive matching. |
| `-` | `-file:_test\.go$` | Negate any atom. |

Example — a definition of `Handler` in Go files, outside tests:

```text
sym:Handler lang:go -file:_test\.go$
```

The badge under the search box always shows the exact compiled query Codebeam sent to the engine, so you can see how your input plus the sidebar filters was interpreted.

## Three search modes

Besides plain text search, the toggles under the search box switch how matching works:

### Normalized search

**Normalized** makes the query ignore case *and* Latin accents: `resume` matches `Résumé`. Useful for prose, comments, and identifiers in accented languages. (A `case:yes` in your query still wins if you ask for it explicitly.)

### Symbols only

**Symbols only** restricts matches to symbol *definitions* — functions, types, methods, classes — instead of every occurrence of the name. Searching `Handler` normally finds hundreds of call sites; with Symbols only, you get the handful of places `Handler` is defined. You can filter further by definition kind (function, class, method…) with the **Symbol kind** facet in the sidebar.

Symbol search is powered by Universal Ctags at indexing time. If it returns nothing, ctags probably wasn't installed when the repository was indexed — see [Troubleshooting](/codebeam/troubleshooting/#search).

### Structural (AST) search

**Structural** matches code by its *syntax tree* rather than its text, using [ast-grep](https://ast-grep.github.io/) patterns. A pattern looks like the code you're looking for, with metavariables as wildcards:

- `$VAR` matches exactly one AST node (an expression, a name, an argument…).
- `$$$` matches any number of nodes (a whole body, an argument list…).

| Pattern (language) | Finds |
| --- | --- |
| `if $ERR != nil { $$$ }` (go) | Every error-check block, regardless of formatting. |
| `foo($$$, ctx)` (go) | Calls to `foo` whose *last* argument is `ctx`. |
| `useEffect(() => { $$$ }, [])` (tsx) | Mount-only React effects. |
| `except Exception: $$$` (python) | Overly broad exception handlers. |

Structural search understands the code, so it isn't fooled by line breaks, spacing, or comments the way a regex is — and each match reports what every `$VAR` captured.

Pick a **language** when you enable structural mode (patterns are parsed per-language). Supported: `bash`, `c`, `go`, `java`, `javascript`, `json`, `python`, `rust`, `tsx`, `typescript`, `yaml`.

Two things to know:

- Structural search scans the **live working tree** for local repositories (uncommitted edits included) and the checked-out primary branch for remote clones. Pass a branch filter to search another indexed branch's committed state instead.
- It scans files rather than a pre-built index, so it is bounded: at most 15 seconds, 5,000 candidate files, and 1,000 matches per search by default (results are flagged when truncated). Narrow with repo/path/language filters for big searches. Admins can tune the bounds — see [Configuration](/codebeam/configuration/#structural-ast-search).

## Filtering and sorting results

The sidebar facets narrow results with one click, and every facet shows live match counts:

- **Source** and **Provider** — local vs. GitHub vs. GitLab, etc.
- **Repository** and **Branch**
- **Language**, **File extension**, and **Top-level path** (first path segment — handy in monorepos)
- **Working tree** — only `dirty` (uncommitted) or only clean, indexed matches
- **Symbol kind** — function, method, class… (with Symbols only)
- **Freshness** — how recently the matching repository was indexed

Sort by relevance (default), repository, path, most/least recently indexed, or match count.

## Reading results

Each result carries provenance so you can trust what you're looking at:

- A **`dirty` badge** means the match comes from a local file with uncommitted changes — you're searching your real working state, not the last commit.
- **Branch badges** show which branch a match is on; identical hits across branches are collapsed into one result with a `+N` badge.
- The **commit hash** each repository was indexed at is attached to every file, so a result is a verifiable `repo:path:line@commit` citation — the same provenance agents get through the API.
- **Freshness** tells you how long ago the repository was indexed.

## The code viewer

Clicking a result opens the file with syntax highlighting:

- **Line anchors** — every line number is a link (`#L42`), so you can share a URL that scrolls straight to the line.
- **Symbol outline** — a sidebar listing the functions, types, and methods defined in the file. Click one to jump to its definition.
- **Find references** — each outline entry has a *refs* link that searches the repository for usages of that symbol.
- **Branch switching** — for remote repositories with multiple indexed branches, view the file as it exists on any of them.
- **File tree** — browse the repository's full tree from the repo page, not just files that matched.
