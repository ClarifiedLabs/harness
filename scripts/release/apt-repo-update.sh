#!/usr/bin/env bash
# Add the .deb packages in DIST_DIR to the reprepro-managed APT repository at
# REPO_DIR (the deb/ subdirectory of the linux-packages checkout). The caller
# must have already imported the repository signing key into the GPG keyring.
#
# Idempotent: packages already present (same name/version/architecture triple)
# are skipped, so re-running a failed publish job is safe. A rebuilt tag with
# the same version but different content is rejected loudly by reprepro.
set -euo pipefail

: "${REPO_DIR:?REPO_DIR is required (reprepro base directory, for example linux-packages/deb)}"

dist_dir="${DIST_DIR:-dist}"
codename="stable"

for tool in reprepro gpg dpkg-deb; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "required tool not found in PATH: ${tool}" >&2
		exit 2
	fi
done

if [[ ! -f "${REPO_DIR}/conf/distributions" ]]; then
	echo "reprepro distributions config not found: ${REPO_DIR}/conf/distributions" >&2
	exit 2
fi

shopt -s nullglob
debs=("${dist_dir}"/*.deb)
if [[ ${#debs[@]} -eq 0 ]]; then
	echo "no .deb packages found in ${dist_dir}" >&2
	exit 1
fi

# already_present reports whether reprepro lists the exact
# name/version/architecture triple. `reprepro list` lines look like:
#   stable|main|amd64: harness 0.5.52
already_present() {
	local arch="$1" package="$2" version="$3"
	reprepro -b "$REPO_DIR" list "$codename" | awk \
		-v want_arch="$arch" \
		-v want_pkg="$package" \
		-v want_ver="$version" '
		{
			head = $1
			sub(/:$/, "", head)
			split(head, fields, "|")
			if (fields[3] == want_arch && $2 == want_pkg && $3 == want_ver) {
				found = 1
			}
		}
		END { exit(found ? 0 : 1) }
	'
}

for deb in "${debs[@]}"; do
	pkg_name="$(dpkg-deb -f "$deb" Package)"
	pkg_version="$(dpkg-deb -f "$deb" Version)"
	pkg_arch="$(dpkg-deb -f "$deb" Architecture)"
	if already_present "$pkg_arch" "$pkg_name" "$pkg_version"; then
		echo "already present, skipping: ${pkg_name} ${pkg_version} ${pkg_arch}"
		continue
	fi
	echo "adding: ${pkg_name} ${pkg_version} ${pkg_arch}"
	reprepro -b "$REPO_DIR" includedeb "$codename" "$deb"
done

# Keep the published public keyring and the Pages marker in step with the
# keyring that actually signed the metadata.
repo_root="$(dirname "$REPO_DIR")"
gpg --batch --armor --export >"${repo_root}/harness-archive-keyring.asc"
touch "${repo_root}/.nojekyll"

reprepro -b "$REPO_DIR" check
reprepro -b "$REPO_DIR" list "$codename"
