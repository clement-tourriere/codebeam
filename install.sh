#!/bin/sh
# Codebeam installer.
#
#   curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
#
# Downloads the latest release binary for the current platform and installs it
# to $CODEBEAM_INSTALL_DIR (default: /usr/local/bin, falling back to
# ~/.local/bin when /usr/local/bin is not writable).
#
# Environment overrides:
#   CODEBEAM_VERSION      install a specific version tag (e.g. v0.2.0)
#   CODEBEAM_INSTALL_DIR  target directory for the binary
set -eu

REPO="clement-tourriere/codebeam"

err() { printf 'error: %s\n' "$1" >&2; exit 1; }
info() { printf '%s\n' "$1"; }

command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar >/dev/null 2>&1 || err "tar is required"

# --- Detect platform ---
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) err "unsupported OS: $os (use the Docker image instead: ghcr.io/$REPO)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac

# --- Resolve version ---
version="${CODEBEAM_VERSION:-}"
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -n1 | cut -d'"' -f4)
  [ -n "$version" ] || err "could not determine the latest release; set CODEBEAM_VERSION"
fi

# --- Download and verify ---
archive="codebeam_${version}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$version/$archive"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

info "Downloading codebeam $version ($os/$arch)..."
curl -fsSL -o "$tmp/$archive" "$url" || err "download failed: $url"

if command -v shasum >/dev/null 2>&1 || command -v sha256sum >/dev/null 2>&1; then
  curl -fsSL -o "$tmp/checksums.txt" \
    "https://github.com/$REPO/releases/download/$version/checksums.txt" || err "checksum download failed"
  (
    cd "$tmp"
    expected=$(grep " $archive\$" checksums.txt | cut -d' ' -f1)
    [ -n "$expected" ] || err "no checksum found for $archive"
    if command -v sha256sum >/dev/null 2>&1; then
      actual=$(sha256sum "$archive" | cut -d' ' -f1)
    else
      actual=$(shasum -a 256 "$archive" | cut -d' ' -f1)
    fi
    [ "$expected" = "$actual" ] || err "checksum mismatch for $archive"
  )
fi

tar -xzf "$tmp/$archive" -C "$tmp" codebeam

# --- Install ---
install_dir="${CODEBEAM_INSTALL_DIR:-/usr/local/bin}"
if [ ! -w "$install_dir" ] && [ -z "${CODEBEAM_INSTALL_DIR:-}" ]; then
  install_dir="$HOME/.local/bin"
fi
mkdir -p "$install_dir" || err "cannot create $install_dir"
[ -w "$install_dir" ] || err "$install_dir is not writable; set CODEBEAM_INSTALL_DIR or rerun with sudo"

install -m 0755 "$tmp/codebeam" "$install_dir/codebeam"
info "Installed codebeam $version to $install_dir/codebeam"

# --- Runtime dependency checks ---
command -v git >/dev/null 2>&1 \
  || info "warning: git is required at runtime (codebeam shells out to it to clone and read repositories) — install it before starting the server"
command -v ctags >/dev/null 2>&1 \
  || info "note: Universal Ctags is optional but recommended for symbol indexing"

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) info "note: $install_dir is not on your PATH" ;;
esac

info "Run 'codebeam' to start the server on http://localhost:8080"
