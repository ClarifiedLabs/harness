#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOOS:?GOOS is required}"
: "${GOARCH:?GOARCH is required}"

stage_dir="${STAGE_DIR:-dist/bin/${GOOS}-${GOARCH}}"
dist_dir="${DIST_DIR:-dist}"
work_dir="${WORK_DIR:-dist/package-tarball}"
name="harness_${VERSION}_${GOOS}_${GOARCH}"
root="${work_dir}/${name}"

rm -rf "$root"
mkdir -p "$root" "$dist_dir"

install -m 0755 "${stage_dir}/harness" "${root}/harness"
install -m 0755 "${stage_dir}/harness-model-proxy" "${root}/harness-model-proxy"
install -m 0755 "${stage_dir}/harness-mcp-proxy" "${root}/harness-mcp-proxy"
install -m 0644 README.md "${root}/README.md"
install -m 0644 LICENSE "${root}/LICENSE"

tar -C "$work_dir" -czf "${dist_dir}/${name}.tar.gz" "$name"
