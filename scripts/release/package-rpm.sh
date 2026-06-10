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

topdir="$(abs_path "${WORK_DIR:-dist/package-rpm}")/rpmbuild"
readme_path="$(abs_path README.md)"
license_path="$(abs_path LICENSE)"
rm -rf "$topdir"
mkdir -p "${topdir}/BUILD" "${topdir}/BUILDROOT" "${topdir}/RPMS" "${topdir}/SOURCES" "${topdir}/SPECS" "$dist_dir"

spec="${topdir}/SPECS/harness.spec"
cat >"$spec" <<SPEC
Name: harness
Version: ${version}
Release: 1%{?dist}
Summary: Tool-using LLM harness CLI
License: MIT
URL: https://github.com/ClarifiedLabs/harness
BuildArch: ${rpm_arch}
AutoReqProv: no

%description
Harness is a small terminal-first CLI for running a provider-neutral,
tool-using LLM loop over local files, shell commands, web fetches, and git.

%prep

%build

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/share/doc/harness
mkdir -p %{buildroot}/usr/share/licenses/harness
install -m 0755 "${stage_dir}/harness" %{buildroot}/usr/bin/harness
install -m 0755 "${stage_dir}/harness-model-proxy" %{buildroot}/usr/bin/harness-model-proxy
install -m 0755 "${stage_dir}/harness-mcp-proxy" %{buildroot}/usr/bin/harness-mcp-proxy
install -m 0644 "${readme_path}" %{buildroot}/usr/share/doc/harness/README.md
install -m 0644 "${license_path}" %{buildroot}/usr/share/licenses/harness/LICENSE

%files
/usr/bin/harness
/usr/bin/harness-model-proxy
/usr/bin/harness-mcp-proxy
%doc /usr/share/doc/harness/README.md
%license /usr/share/licenses/harness/LICENSE
SPEC

rpmbuild --define "_topdir ${topdir}" --target "${rpm_arch}" -bb "$spec"
rpm_path="$(find "${topdir}/RPMS" -type f -name '*.rpm' -print | sort | awk 'END {print}')"
if [[ -z "$rpm_path" ]]; then
	echo "rpmbuild did not produce an rpm" >&2
	exit 1
fi
cp "$rpm_path" "${dist_dir}/harness-${version}-1.${rpm_arch}.rpm"
