#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required (for example, v1.2.3)}"

if [[ "$VERSION" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
	version="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"
else
	echo "VERSION must be a v-prefixed semantic version (for example, v1.2.3)" >&2
	exit 2
fi

readme="${1:-README.md}"
if [[ ! -f "$readme" ]]; then
	echo "README not found: $readme" >&2
	exit 1
fi

block_file="$(mktemp)"
output_file="$(mktemp)"
trap 'rm -f "$block_file" "$output_file"' EXIT

cat >"$block_file" <<EOF
Or download the latest signed package containing all three binaries:

- Apple silicon (arm64): [\`.pkg\` (${VERSION})](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness_${VERSION}_darwin_arm64.pkg)

### Linux

On Debian/Ubuntu, install from the APT repository (amd64 and arm64):

\`\`\`sh
curl -fsSL https://clarifiedlabs.github.io/linux-packages/clarifiedlabs-archive-keyring.asc | sudo gpg --dearmor -o /usr/share/keyrings/clarifiedlabs-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/clarifiedlabs-archive-keyring.gpg] https://clarifiedlabs.github.io/linux-packages/deb stable main" | sudo tee /etc/apt/sources.list.d/clarifiedlabs.list
sudo apt-get update
sudo apt-get install harness harness-model-proxy harness-mcp-proxy
\`\`\`

On Fedora/RHEL, install from the RPM repository:

\`\`\`sh
sudo curl -fsSL -o /etc/yum.repos.d/clarifiedlabs.repo https://clarifiedlabs.github.io/linux-packages/clarifiedlabs.repo
sudo dnf install harness harness-model-proxy harness-mcp-proxy
\`\`\`

Or download the latest release, ${VERSION}, as individual packages:

| Binary | amd64 / x86_64 | arm64 / aarch64 |
|---|---|---|
| \`harness\` | [\`.deb\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness_${version}_amd64.deb) · [\`.rpm\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-${version}-1.x86_64.rpm) | [\`.deb\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness_${version}_arm64.deb) · [\`.rpm\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-${version}-1.aarch64.rpm) |
| \`harness-model-proxy\` | [\`.deb\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-model-proxy_${version}_amd64.deb) · [\`.rpm\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-model-proxy-${version}-1.x86_64.rpm) | [\`.deb\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-model-proxy_${version}_arm64.deb) · [\`.rpm\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-model-proxy-${version}-1.aarch64.rpm) |
| \`harness-mcp-proxy\` | [\`.deb\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-mcp-proxy_${version}_amd64.deb) · [\`.rpm\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-mcp-proxy-${version}-1.x86_64.rpm) | [\`.deb\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-mcp-proxy_${version}_arm64.deb) · [\`.rpm\`](https://github.com/ClarifiedLabs/harness/releases/download/${VERSION}/harness-mcp-proxy-${version}-1.aarch64.rpm) |

### Docker

Multi-architecture images are available for Linux amd64 and arm64:

- [\`ghcr.io/clarifiedlabs/harness:${version}\`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness)
- [\`ghcr.io/clarifiedlabs/harness-model-proxy:${version}\`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-model-proxy)
- [\`ghcr.io/clarifiedlabs/harness-mcp-proxy:${version}\`](https://github.com/ClarifiedLabs/harness/pkgs/container/harness-mcp-proxy)
EOF

awk -v block_file="$block_file" '
	BEGIN {
		while ((getline line < block_file) > 0) {
			block = block line ORS
		}
		close(block_file)
	}
	$0 == "<!-- release-artifacts:start -->" {
		starts++
		print
		printf "%s", block
		replacing = 1
		next
	}
	$0 == "<!-- release-artifacts:end -->" {
		ends++
		replacing = 0
		print
		next
	}
	!replacing { print }
	END {
		if (starts != 1 || ends != 1 || replacing) {
			print "expected exactly one complete release-artifacts marker pair" > "/dev/stderr"
			exit 1
		}
	}
' "$readme" >"$output_file"

cp "$output_file" "$readme"
