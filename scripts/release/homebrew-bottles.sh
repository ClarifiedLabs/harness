#!/usr/bin/env bash
set -euo pipefail

stage_release=false
if [[ "${1:-}" == --stage-release ]]; then
  stage_release=true
  shift
fi
bottles_dir="$(cd "${1:?bottle artifact directory is required}" && pwd)"

: "${TAG:?TAG is required}"
version="${TAG#v}"
root_url="https://github.com/${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}/releases/download/${TAG}"
formulas=(harness harness-model-proxy harness-mcp-proxy harness-full)
platforms=(arm64_tahoe arm64_linux x86_64_linux)
qualified_formulas=()
bottle_jsons=()
local_archives=()
release_archives=()

for formula in "${formulas[@]}"; do
  qualified="clarifiedlabs/tap/${formula}"
  qualified_formulas+=("$qualified")
  for platform in "${platforms[@]}"; do
    base="${formula}--${version}.${platform}.bottle"
    metadata="${bottles_dir}/${base}.json"
    archive="${base}.tar.gz"
    release_name="${formula}-${version}.${platform}.bottle.tar.gz"
    if ! entry="$(jq -er --arg formula "$qualified" --arg version "$version" \
      --arg root "$root_url" --arg platform "$platform" --arg archive "$archive" \
      --arg release "$release_name" '
        select(keys == [$formula]) | .[$formula]
        | select(.formula.pkg_version == $version and .bottle.root_url == $root)
        | .bottle.tags | select(keys == [$platform]) | .[$platform]
        | select(.local_filename == $archive and .filename == $release)
        | select(.sha256 | type == "string" and test("^[0-9a-f]{64}$"))
        | [.sha256, .filename] | @tsv
      ' "$metadata")"; then
      echo "Invalid or missing bottle metadata: $metadata" >&2
      exit 1
    fi
    IFS=$'\t' read -r checksum published_archive <<< "$entry"
    # Hash stdin: some sha256sum implementations lack GNU's --status.
    if ! digest="$(sha256sum < "${bottles_dir}/${archive}")" ||
      [[ "${digest%%[[:space:]]*}" != "$checksum" ]]; then
      echo "Missing or mismatched bottle archive: $archive" >&2
      exit 1
    fi
    bottle_jsons+=("$metadata")
    release_archives+=("$published_archive")
    if "$stage_release"; then
      target="${bottles_dir}/${published_archive}"
      if [[ -e "$target" || -L "$target" ]]; then
        echo "Release bottle archive already exists: $target" >&2
        exit 1
      fi
      local_archives+=("${bottles_dir}/${archive}")
    fi
  done
done

if "$stage_release"; then
  # Rename only the release job's copies. The tap jobs download the original
  # artifacts, and Homebrew's merge still needs the original metadata.
  for i in "${!local_archives[@]}"; do
    mv -- "${local_archives[i]}" "${bottles_dir}/${release_archives[i]}"
  done
  exit 0
fi

if [[ "${VERIFY_RELEASE_DOWNLOADS:-0}" == 1 ]]; then
  for archive in "${release_archives[@]}"; do
    if ! curl --head --location --fail --silent --show-error \
      --connect-timeout 10 --max-time 20 --retry 3 --retry-all-errors --retry-max-time 45 \
      "${root_url}/${archive}" >/dev/null; then
      echo "Public release bottle unavailable: ${root_url}/${archive}" >&2
      exit 1
    fi
  done
fi

cd "${TAP_DIR:?TAP_DIR is required}"
# A merge writes the entire bottle block, so supply all platforms at once.
brew bottle --merge --write --no-commit --no-all-checks "${bottle_jsons[@]}"
formula_info="$(brew info --json=v2 "${qualified_formulas[@]}")"

if ! jq -e --slurpfile info <(printf '%s\n' "$formula_info") '
  # Compare every rendered bottle checksum and download URL to the input.
  (reduce .[] as $metadata ({}; . * ($metadata | with_entries(
    .value.bottle as $bottle | .value = {
      root: $bottle.root_url,
      files: ($bottle.tags | map_values({sha256, url: ($bottle.root_url + "/" + .filename)}))
    }
  )))) as $expected
  | ($info[0].formulae | map({
      key: .full_name,
      value: {
        root: .bottle.stable.root_url,
        files: (.bottle.stable.files | map_values({sha256, url}))
      }
    }) | from_entries) == $expected
' -s "${bottle_jsons[@]}" >/dev/null; then
  echo "Merged formulas do not advertise the expected bottle platforms, checksums and URLs" >&2
  exit 1
fi
