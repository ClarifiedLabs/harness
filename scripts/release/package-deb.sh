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

mkdir -p "$dist_dir"

package_one() {
	local name="$1"
	local binary="$2"
	local summary="$3"
	local description="$4"
	local pkgroot="${WORK_DIR:-dist/package-deb}/${name}_${version}_${deb_arch}"

	rm -rf "$pkgroot"
	mkdir -p "${pkgroot}/DEBIAN" "${pkgroot}/usr/bin" "${pkgroot}/usr/share/doc/${name}"

	install -m 0755 "${stage_dir}/${binary}" "${pkgroot}/usr/bin/${binary}"
	install -m 0644 README.md "${pkgroot}/usr/share/doc/${name}/README.md"
	install -m 0644 LICENSE "${pkgroot}/usr/share/doc/${name}/LICENSE"

	local installed_size
	installed_size="$(du -sk "${pkgroot}/usr" | awk '{print $1}')"
	cat >"${pkgroot}/DEBIAN/control" <<CONTROL
Package: ${name}
Version: ${version}
Section: utils
Priority: optional
Architecture: ${deb_arch}
Maintainer: Clarified Labs <opensource@clarifiedlabs.com>
Installed-Size: ${installed_size}
Homepage: https://github.com/ClarifiedLabs/harness
Description: ${summary}
 ${description}
CONTROL

	dpkg-deb --build --root-owner-group "$pkgroot" "${dist_dir}/${name}_${version}_${deb_arch}.deb"
}

package_one \
	"harness" \
	"harness" \
	"Tool-using LLM harness CLI" \
	"Harness is a terminal-first CLI for running a provider-neutral, tool-using LLM loop over local files, shell commands, web fetches, and git."
package_one \
	"harness-model-proxy" \
	"harness-model-proxy" \
	"Provider and model proxy for harness" \
	"The harness model proxy owns provider configuration, API keys, model catalog metadata, and concrete provider calls."
package_one \
	"harness-mcp-proxy" \
	"harness-mcp-proxy" \
	"MCP proxy for harness" \
	"The harness MCP proxy supervises configured MCP servers and exposes their merged tool surface over HTTP or stdio."
