#!/usr/bin/env bash
# Builds the ast-grep WASI shim and copies it to internal/structural/astgrep.wasm.
#
# Requirements:
#   - Rust with the wasm32-wasip1 target (rustup target add wasm32-wasip1)
#   - wasi-sdk (for compiling the tree-sitter C grammars); auto-downloaded to
#     ~/.cache/wasi-sdk when missing, or point WASI_SDK at an existing install.
#   - wasm-opt (optional; shrinks the module when present)
set -euo pipefail
cd "$(dirname "$0")"

WASI_SDK_VERSION="${WASI_SDK_VERSION:-33}"

detect_wasi_sdk() {
    if [ -n "${WASI_SDK:-}" ]; then
        return
    fi
    local os arch
    case "$(uname -s)" in
        Darwin) os="macos" ;;
        Linux) os="linux" ;;
        *) echo "error: unsupported OS $(uname -s); set WASI_SDK manually" >&2; exit 1 ;;
    esac
    case "$(uname -m)" in
        arm64 | aarch64) arch="arm64" ;;
        x86_64) arch="x86_64" ;;
        *) echo "error: unsupported arch $(uname -m); set WASI_SDK manually" >&2; exit 1 ;;
    esac
    local name="wasi-sdk-${WASI_SDK_VERSION}.0-${arch}-${os}"
    WASI_SDK="$HOME/.cache/wasi-sdk/$name"
    if [ ! -d "$WASI_SDK" ]; then
        echo "downloading $name to ~/.cache/wasi-sdk ..."
        mkdir -p "$HOME/.cache/wasi-sdk"
        curl -sSfL "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-${WASI_SDK_VERSION}/${name}.tar.gz" |
            tar xz -C "$HOME/.cache/wasi-sdk"
    fi
}

detect_wasi_sdk

if ! rustup target list --installed 2>/dev/null | grep -q wasm32-wasip1; then
    echo "error: wasm32-wasip1 target missing; run: rustup target add wasm32-wasip1" >&2
    exit 1
fi

export CC_wasm32_wasip1="$WASI_SDK/bin/clang"
export AR_wasm32_wasip1="$WASI_SDK/bin/llvm-ar"
# SIMD/bulk-memory/nontrapping-fptoint are all supported by wazero and speed
# up the tree-sitter lexer loops measurably.
WASM_FEATURES="-msimd128 -mbulk-memory -mnontrapping-fptoint"
export CFLAGS_wasm32_wasip1="--sysroot=$WASI_SDK/share/wasi-sysroot -O3 $WASM_FEATURES"
export RUSTFLAGS="${RUSTFLAGS:-} -C target-feature=+simd128,+bulk-memory,+nontrapping-fptoint"

cargo build --release --target wasm32-wasip1

OUT=target/wasm32-wasip1/release/astgrep_shim.wasm
DEST=../../internal/structural/astgrep.wasm

if command -v wasm-opt >/dev/null 2>&1; then
    echo "running wasm-opt ..."
    wasm-opt -O3 --enable-simd --enable-bulk-memory --enable-nontrapping-float-to-int --strip-debug "$OUT" -o "$OUT.opt"
    mv "$OUT.opt" "$OUT"
else
    echo "note: wasm-opt not found (brew install binaryen); shipping unoptimized module"
fi

mkdir -p "$(dirname "$DEST")"
cp "$OUT" "$DEST"
ls -lh "$DEST"
