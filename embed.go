// Package codebeam embeds the web UI assets (HTML templates and the built
// static files) so a released binary is fully self-contained. The server
// prefers the on-disk paths when they exist — during development and in the
// Docker image — and falls back to these embedded copies otherwise.
//
// static/app.css and static/htmx.min.js are build artifacts (mise run
// css:build); release builds must generate them before `go build` or the
// embedded UI ships without styling.
package codebeam

import "embed"

//go:embed templates/*.html
var Templates embed.FS

//go:embed all:static
var Static embed.FS
