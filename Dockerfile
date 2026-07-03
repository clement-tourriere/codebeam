# syntax=docker/dockerfile:1

# --- Frontend: Tailwind/DaisyUI CSS + HTMX vendoring (writes into static/) ---
FROM node:22-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json frontend/
RUN npm --prefix frontend ci
# Tailwind v4 auto-detects class usage from the surrounding source tree, so the
# templates (and the rest of the repo) must be present when the CSS is built.
COPY . .
RUN npm --prefix frontend run build

# --- Backend: static Go binary (modernc sqlite is pure Go, so CGO stays off) ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/codebeam ./cmd/codebeam

# --- Runtime ---
# Not scratch: codebeam shells out to `git` for clone/fetch/read, and Universal
# Ctags (Alpine's `ctags` package) enables symbol indexing. Both need a real OS.
FROM alpine:3.22
RUN apk add --no-cache git ca-certificates tzdata ctags \
    && adduser -D -u 1000 codebeam \
    && mkdir -p /data /config \
    && chown codebeam:codebeam /data /config

COPY --from=build /out/codebeam /usr/local/bin/codebeam
COPY --from=frontend /src/static /app/static
COPY templates /app/templates

# /data holds the SQLite DB, cloned repos, and search indexes. /config holds the
# auto-generated token-encryption key — deliberately a separate volume so a
# stolen data backup does not also carry the key (XDG_CONFIG_HOME steers
# os.UserConfigDir there). Losing /config makes stored code-host tokens
# unreadable, so persist both.
ENV CODEBEAM_DATA_DIR=/data \
    CODEBEAM_STATIC_DIR=/app/static \
    CODEBEAM_TEMPLATE_GLOB=/app/templates/*.html \
    XDG_CONFIG_HOME=/config \
    HOME=/home/codebeam
# Bind-mounted repos may be owned by a different UID than the container user;
# scoped to this single-purpose container, trusting them is fine.
ENV GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0=safe.directory \
    GIT_CONFIG_VALUE_0=*

USER codebeam
VOLUME ["/data", "/config"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
    CMD wget -qO /dev/null http://127.0.0.1:8080/ || exit 1

ENTRYPOINT ["codebeam"]
