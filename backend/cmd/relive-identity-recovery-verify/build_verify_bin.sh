#!/usr/bin/env bash
# build_verify_bin.sh
# Build the independent Linux/macOS verify binary with main.buildSourceDigest injected.
# Usage:
#   ./build_verify_bin.sh [output-path]
# Env:
#   SOURCE_DIGEST  optional override; otherwise computed via source_digest.py
#   GOOS/GOARCH    optional cross-compile targets
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BACKEND_DIR="$REPO_ROOT/backend"
DIGEST_SCRIPT="$SCRIPT_DIR/source_digest.py"
OUT="${1:-$SCRIPT_DIR/../../relive-identity-recovery-verify.bin}"

if [[ -d /Library/Developer/CommandLineTools && -z "${DEVELOPER_DIR:-}" ]]; then
  export DEVELOPER_DIR=/Library/Developer/CommandLineTools
fi

if [[ -z "${SOURCE_DIGEST:-}" ]]; then
  SOURCE_DIGEST="$(python3 "$DIGEST_SCRIPT" --repo-root "$REPO_ROOT" --digest-only)"
fi

if [[ ! "$SOURCE_DIGEST" =~ ^[a-f0-9]{64}$ ]]; then
  echo "build_verify_bin: error: invalid SOURCE_DIGEST=$SOURCE_DIGEST" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUT")"

echo "build_verify_bin: SOURCE_DIGEST=$SOURCE_DIGEST"
echo "build_verify_bin: OUT=$OUT"
echo "build_verify_bin: GOOS=${GOOS:-$(go env GOOS)} GOARCH=${GOARCH:-$(go env GOARCH)} CGO_ENABLED=${CGO_ENABLED:-1}"

(
  cd "$BACKEND_DIR"
  CGO_ENABLED="${CGO_ENABLED:-1}" go build -tags fts5 \
    -ldflags "-X main.buildSourceDigest=${SOURCE_DIGEST}" \
    -o "$OUT" \
    ./cmd/relive-identity-recovery-verify
)

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$OUT"
elif command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "$OUT"
fi

echo "build_verify_bin: ok"
