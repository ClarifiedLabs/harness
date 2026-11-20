#!/usr/bin/env bash
set -euo pipefail

bottles_dir="$(cd "${1:?bottle artifact directory is required}" && pwd)"
cd "${TAP_DIR:?TAP_DIR is required}"

: "${TAG:?TAG is required}"
version="${TAG#v}"
root_url="https://github.com/${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}/releases/download/${TAG}"
formulas=(harness harness-model-proxy harness-mcp-proxy harness-full)
platforms=(arm64_tahoe arm64_linux x86_64_linux)
qualified_formulas=()
bottle_jsons=()

for formula in "${formulas[@]}"; do
  qualified="clarifiedlabs/tap/${formula}"
  qualified_formulas+=("$qualified")
  for platform in "${platforms[@]}"; do
    base="${formula}--${version}.${platform}.bottle"
    metadata="${bottles_dir}/${base}.json"
    archive="${base}.tar.gz"
    if ! checksum="$(jq -er --arg formula "$qualified" --arg version "$version" \
      --arg root "$root_url" --arg platform "$platform" --arg archive "$archive" '
        select(keys == [$formula]) | .[$formula]
        | select(.formula.pkg_version == $version and .bottle.root_url == $root)
        | .bottle.tags | select(keys == [$platform]) | .[$platform]
        | select(.local_filename == $archive) | .sha256
        | select(type == "string" and test("^[0-9a-f]{64}$"))
      ' "$metadata")"; then
      echo "Invalid or missing bottle metadata: $metadata" >&2
      exit 1
    fi
    # Hash stdin: some sha256sum implementations lack GNU's --status.
    if ! digest="$(sha256sum < "${bottles_dir}/${archive}")" ||
      [[ "${digest%%[[:space:]]*}" != "$checksum" ]]; then
      echo "Missing or mismatched bottle archive: $archive" >&2
      exit 1
    fi
    bottle_jsons+=("$metadata")
  done
done

# A merge writes the entire bottle block, so supply all platforms at once.
brew bottle --merge --write --no-commit --no-all-checks "${bottle_jsons[@]}"
formula_info="$(brew info --json=v2 "${qualified_formulas[@]}")"

if ! jq -e --slurpfile info <(printf '%s\n' "$formula_info") '
  # Compare the rendered Homebrew metadata to the full set of input checksums.
  (reduce .[] as $metadata ({}; . * ($metadata | with_entries(
    .value = {
      root: .value.bottle.root_url,
      sha: (.value.bottle.tags | map_values(.sha256))
    }
  )))) as $expected
  | ($info[0].formulae | map({
      key: .full_name,
      value: {
        root: .bottle.stable.root_url,
        sha: (.bottle.stable.files | map_values(.sha256))
      }
    }) | from_entries) == $expected
' -s "${bottle_jsons[@]}" >/dev/null; then
  echo "Merged formulas do not advertise the expected bottle platforms and checksums" >&2
  exit 1
fi
