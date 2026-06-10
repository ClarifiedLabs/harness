#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${COMMIT:?COMMIT is required}"
: "${DATE:?DATE is required}"
: "${GOOS:?GOOS is required}"
: "${GOARCH:?GOARCH is required}"

out_dir="${OUT_DIR:-dist/bin/${GOOS}-${GOARCH}}"
mkdir -p "$out_dir"

ldflags="-s -w"
ldflags+=" -X harness/internal/buildinfo.Version=${VERSION}"
ldflags+=" -X harness/internal/buildinfo.Commit=${COMMIT}"
ldflags+=" -X harness/internal/buildinfo.Date=${DATE}"

build_one() {
	local name="$1"
	local pkg="$2"
	CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "$ldflags" -o "${out_dir}/${name}" "$pkg"
}

build_one harness ./cmd/harness
build_one harness-model-proxy ./cmd/harness-model-proxy
build_one harness-mcp-proxy ./cmd/harness-mcp-proxy
