#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOARCH:?GOARCH is required}"

stage_dir="${STAGE_DIR:-dist/bin/linux-${GOARCH}}"
dist_dir="${DIST_DIR:-dist}"
version="${VERSION#v}"

case "$GOARCH" in
	amd64) deb_arch="amd64" ;;
	arm64) deb_arch="arm64" ;;
	*) echo "unsupported GOARCH for deb: ${GOARCH}" >&2; exit 2 ;;
esac

pkgroot="${WORK_DIR:-dist/package-deb}/harness_${version}_${deb_arch}"
rm -rf "$pkgroot"
mkdir -p "${pkgroot}/DEBIAN" "${pkgroot}/usr/bin" "${pkgroot}/usr/share/doc/harness"

install -m 0755 "${stage_dir}/harness" "${pkgroot}/usr/bin/harness"
install -m 0755 "${stage_dir}/harness-model-proxy" "${pkgroot}/usr/bin/harness-model-proxy"
install -m 0755 "${stage_dir}/harness-mcp-proxy" "${pkgroot}/usr/bin/harness-mcp-proxy"
install -m 0644 README.md "${pkgroot}/usr/share/doc/harness/README.md"
install -m 0644 LICENSE "${pkgroot}/usr/share/doc/harness/LICENSE"

installed_size="$(du -sk "${pkgroot}/usr" | awk '{print $1}')"
cat >"${pkgroot}/DEBIAN/control" <<CONTROL
Package: harness
Version: ${version}
Section: utils
Priority: optional
Architecture: ${deb_arch}
Maintainer: Clarified Labs <opensource@clarifiedlabs.com>
Installed-Size: ${installed_size}
Homepage: https://github.com/ClarifiedLabs/harness
Description: Tool-using LLM harness CLI
 Harness is a small terminal-first CLI for running a provider-neutral,
 tool-using LLM loop over local files, shell commands, web fetches, and git.
CONTROL

mkdir -p "$dist_dir"
dpkg-deb --build --root-owner-group "$pkgroot" "${dist_dir}/harness_${version}_${deb_arch}.deb"
