#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOARCH:?GOARCH is required}"

repo_root="$(pwd -P)"

abs_path() {
	case "$1" in
		/*) printf '%s\n' "$1" ;;
		*) printf '%s/%s\n' "$repo_root" "$1" ;;
	esac
}

stage_dir="$(abs_path "${STAGE_DIR:-dist/bin/linux-${GOARCH}}")"
dist_dir="$(abs_path "${DIST_DIR:-dist}")"
version="${VERSION#v}"

case "$GOARCH" in
	amd64) rpm_arch="x86_64" ;;
	arm64) rpm_arch="aarch64" ;;
	*) echo "unsupported GOARCH for rpm: ${GOARCH}" >&2; exit 2 ;;
esac

work_dir="$(abs_path "${WORK_DIR:-dist/package-rpm}")"
readme_path="$(abs_path README.md)"
license_path="$(abs_path LICENSE)"
rm -rf "$work_dir"
mkdir -p "$dist_dir"

package_one() {
	local name="$1"
	local binary="$2"
	local summary="$3"
	local description="$4"
	local topdir="${work_dir}/${name}/rpmbuild"
	local spec="${topdir}/SPECS/${name}.spec"

	mkdir -p "${topdir}/BUILD" "${topdir}/BUILDROOT" "${topdir}/RPMS" "${topdir}/SOURCES" "${topdir}/SPECS"
	cat >"$spec" <<SPEC
Name: ${name}
Version: ${version}
Release: 1%{?dist}
Summary: ${summary}
License: MIT
URL: https://github.com/ClarifiedLabs/harness
BuildArch: ${rpm_arch}
AutoReqProv: no

%description
${description}

%prep

%build

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/doc/${name}
mkdir -p %{buildroot}/usr/share/licenses/${name}
install -m 0755 "${stage_dir}/${binary}" %{buildroot}/usr/bin/${binary}
install -m 0644 "${readme_path}" %{buildroot}/usr/share/doc/${name}/README.md
install -m 0644 "${license_path}" %{buildroot}/usr/share/licenses/${name}/LICENSE

%files
/usr/bin/${binary}
%doc /usr/share/doc/${name}/README.md
%license /usr/share/licenses/${name}/LICENSE
SPEC

	rpmbuild --define "_topdir ${topdir}" --target "${rpm_arch}" -bb "$spec"
	local rpm_path
	rpm_path="$(find "${topdir}/RPMS" -type f -name '*.rpm' -print | sort | awk 'END {print}')"
	if [[ -z "$rpm_path" ]]; then
		echo "rpmbuild did not produce an rpm for ${name}" >&2
		exit 1
	fi
	cp "$rpm_path" "${dist_dir}/${name}-${version}-1.${rpm_arch}.rpm"
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
