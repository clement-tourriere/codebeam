## v0.5.1 (2026-07-22)

### Fix

- **docker**: embed the freshly built CSS in the Go binary

## v0.5.0 (2026-07-22)

### Feat

- **search**: exclude facet values across UI, API, CLI, and MCP

### Fix

- **web**: cache-bust static assets with a content hash
- **tests**: disable global git hooks in repo-building test helpers

## v0.4.0 (2026-07-21)

### Feat

- **cli**: add cb mcp, a stdio MCP proxy to the server

### Fix

- **docker**: run tini as PID 1 to reap orphaned git helpers

## v0.3.0 (2026-07-21)

### Feat

- **cli**: log in through Cloudflare Access-protected servers

### Fix

- **auth**: refresh expiring code host OAuth tokens
- **docker**: support PaaS builders and volumes

## v0.2.1 (2026-07-21)

### Fix

- **indexing**: keep search available during reindex

## v0.2.0 (2026-07-07)

### Feat

- **cli**: add cb, a command-line client for any Codebeam server

## v0.1.1 (2026-07-03)

### Fix

- **release**: add MIT license and ship it in the release archives

## v0.1.0 (2026-07-03)

### Feat

- add CI, commitizen release flow, curl installer, and docs deploy

## v0.0.0 (2026-07-03)
