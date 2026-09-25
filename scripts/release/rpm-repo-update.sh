#!/usr/bin/env bash
# Add the .rpm packages in DIST_DIR to the createrepo_c-managed RPM repository
# under REPO_DIR (the linux-packages checkout root) and GPG-sign repomd.xml
# for every arch directory that received new packages. The caller must have
# already imported the repository signing key into the GPG keyring.
#
# Idempotent: packages already present with identical content are skipped and
# metadata is only regenerated for arch directories that changed, so re-running
# a failed publish job produces no new commit. A rebuilt tag with the same
# version but different content is a hard error: never silently replace signed
# artifacts.
set -euo pipefail

: "${REPO_DIR:?REPO_DIR is required (linux-packages checkout root)}"

dist_dir="${DIST_DIR:-dist}"

for tool in createrepo_c gpg; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "required tool not found in PATH: ${tool}" >&2
		exit 2
	fi
done

shopt -s nullglob
rpms=("${dist_dir}"/*.rpm)
if [[ ${#rpms[@]} -eq 0 ]]; then
	echo "no .rpm packages found in ${dist_dir}" >&2
	exit 1
fi

declare -A changed_arches=()

for rpm_file in "${rpms[@]}"; do
	case "$rpm_file" in
		*.x86_64.rpm) rpm_arch="x86_64" ;;
		*.aarch64.rpm) rpm_arch="aarch64" ;;
		*)
			echo "cannot determine target arch from rpm filename: ${rpm_file}" >&2
			exit 2
			;;
	esac
	mkdir -p "${REPO_DIR}/rpm/${rpm_arch}"
	dest="${REPO_DIR}/rpm/${rpm_arch}/$(basename "$rpm_file")"
	if [[ ! -e "$dest" ]]; then
		cp "$rpm_file" "$dest"
		changed_arches["$rpm_arch"]=1
		echo "added: ${dest}"
	elif cmp -s "$rpm_file" "$dest"; then
		echo "already present, skipping: ${dest}"
	else
		echo "refusing to replace differing package: ${dest} (rebuilt tag with the same version?)" >&2
		exit 1
	fi
done

if [[ ${#changed_arches[@]} -gt 0 ]]; then
	for rpm_arch in "${!changed_arches[@]}"; do
		arch_dir="${REPO_DIR}/rpm/${rpm_arch}"
		createrepo_c "$arch_dir"
		(
			cd "$arch_dir"
			gpg --batch --yes --armor --detach-sign \
				-o repodata/repomd.xml.asc repodata/repomd.xml
		)
		echo "regenerated and signed repodata for ${rpm_arch}"
	done
else
	echo "no new packages; repository metadata left untouched"
fi

shopt -s nullglob
repomds=("${REPO_DIR}"/rpm/*/repodata/repomd.xml)
if [[ ${#repomds[@]} -eq 0 ]]; then
	echo "no rpm repository metadata found under ${REPO_DIR}/rpm" >&2
	exit 1
fi
for repomd in "${repomds[@]}"; do
	gpg --batch --verify "${repomd}.asc" "$repomd"
done
