# Codebeam

Local-first, Sourcegraph-style code search backed by [Zoekt](https://github.com/sourcegraph/zoekt) — lexical, symbol, and structural (AST) search over your repositories, with a web UI, a CLI, and an MCP server for coding agents. Ships as a single self-contained binary.

**Full documentation: <https://clement-tourriere.github.io/codebeam/>**

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
codebeam            # web UI on http://localhost:8080
```

Or with Docker:

```sh
docker run -d --name codebeam -p 8080:8080 \
  -v codebeam-data:/data -v codebeam-config:/config \
  ghcr.io/clement-tourriere/codebeam:latest
```

`git` must be on `PATH`; [Universal Ctags](https://github.com/universal-ctags/ctags) is optional and enables symbol search. Per-platform binaries are on the [releases page](https://github.com/clement-tourriere/codebeam/releases).

## Search from the terminal

The release archive also ships `cb`, a client for any Codebeam server:

```sh
cb login codebeam.example.com     # one-time browser sign-in
cb search "func NewServer" --lang go
cb def NewServer                  # where is it defined?
cb read local/app:cmd/main.go:1-40
```

Headless (agents, CI): set `CODEBEAM_URL` and `CODEBEAM_TOKEN` to a personal access token — no login needed. Instances behind Cloudflare Access are detected and handled automatically. See the [CLI docs](https://clement-tourriere.github.io/codebeam/cli/).

## Give it to your agent

```sh
claude mcp add codebeam -- codebeam mcp                                  # local index, stdio
claude mcp add --transport http codebeam https://your-host/mcp           # shared server, OAuth
```

Eight retrieval tools (search, symbols, references, AST patterns, file reading, stats) backed by the same engines as the web UI, scoped to each user's repositories. See [integrations](https://clement-tourriere.github.io/codebeam/integrations/).

## Develop

```sh
mise install
mise run dev        # http://localhost:8080, data under .codebeam/
mise run test
```

Releases follow [Conventional Commits](https://www.conventionalcommits.org/) via commitizen: `mise run release`.

## License

[MIT](LICENSE). Notable dependencies: [Zoekt](https://github.com/sourcegraph/zoekt) (Apache-2.0), [wazero](https://github.com/tetratelabs/wazero) (Apache-2.0), [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (BSD-3-Clause), [ast-grep](https://github.com/ast-grep/ast-grep) (MIT).
