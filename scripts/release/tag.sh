#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required; use patch, minor, major, or x.y.z}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "${AUTOPUSH:-0}" in
	"" | 0 | false | FALSE | no | NO)
		push=0
		;;
	1 | true | TRUE | yes | YES)
		push=1
		;;
	*)
		echo "AUTOPUSH must be 1/true/yes or 0/false/no" >&2
		exit 2
		;;
esac

"${script_dir}/check-clean.sh"

latest_tag=""
while IFS= read -r tag; do
	if [[ "$tag" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
		latest_tag="$tag"
		break
	fi
done < <(git tag --list 'v*' --sort=-v:refname)

current_major=0
current_minor=0
current_patch=0
if [[ -n "$latest_tag" ]]; then
	if [[ "$latest_tag" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
		current_major="${BASH_REMATCH[1]}"
		current_minor="${BASH_REMATCH[2]}"
		current_patch="${BASH_REMATCH[3]}"
	fi
fi

target_major="$current_major"
target_minor="$current_minor"
target_patch="$current_patch"

case "$VERSION" in
	patch)
		target_patch=$((current_patch + 1))
		;;
	minor)
		target_minor=$((current_minor + 1))
		target_patch=0
		;;
	major)
		target_major=$((current_major + 1))
		target_minor=0
		target_patch=0
		;;
	*)
		if [[ "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
			target_major="${BASH_REMATCH[1]}"
			target_minor="${BASH_REMATCH[2]}"
			target_patch="${BASH_REMATCH[3]}"
		else
			echo "VERSION must be patch, minor, major, or x.y.z" >&2
			exit 2
		fi
		;;
esac

if (( target_major < current_major ||
	(target_major == current_major && target_minor < current_minor) ||
	(target_major == current_major && target_minor == current_minor && target_patch <= current_patch) )); then
	echo "target version ${target_major}.${target_minor}.${target_patch} must be greater than latest ${current_major}.${current_minor}.${current_patch}" >&2
	exit 2
fi

tag="v${target_major}.${target_minor}.${target_patch}"
if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
	echo "tag ${tag} already exists" >&2
	exit 1
fi

commit="$(git rev-parse --verify HEAD)"
printf 'Creating annotated release tag %s at %s\n' "$tag" "$commit"
git tag -a "$tag" -m "release: ${tag}"

if [[ "$push" -eq 1 ]]; then
	git remote get-url origin >/dev/null
	printf 'Pushing %s to origin\n' "$tag"
	git push origin "$tag"
else
	printf 'Created %s locally. Push it with: git push origin %s\n' "$tag" "$tag"
fi
