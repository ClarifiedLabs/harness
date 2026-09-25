# Release

Release builds produce the three shipped binaries:

- `harness`
- `harness-model-proxy`
- `harness-mcp-proxy`

The apps support `--version` (the proxies also accept a `version` subcommand):

```sh
harness --version
harness-model-proxy --version
harness-mcp-proxy --version
```

Release builds inject the repository tag (`v*`) into those commands. The MCP
protocol version is separate and is shown by both MCP-proxy version spellings
and by `harness lsp version`. Persisted session `state.json` files use schema
version `3`. Earlier schemas are rejected rather than migrated.

## Artifacts

Pushing a `v*` tag runs `.github/workflows/release.yml`. The workflow builds:

- macOS arm64 on `macos-26`
- Linux amd64
- Linux arm64

It publishes tarballs, per-binary `.deb` and `.rpm` packages, per-binary GHCR
container images, a signed and notarized macOS `.pkg`, Homebrew bottles for
macOS arm64, macOS Intel, Linux amd64, and Linux arm64, SHA-256 checksums, and
GitHub artifact attestations. On `v*` tag builds the `.rpm` packages are
GPG-signed in `build-linux` before attestation and upload, so the GitHub
release assets, `checksums.txt`, attestations, and the package repositories
below all serve identical signed files. Dry-run RPMs stay unsigned, matching
the unsigned macOS `.pkg` dry-run precedent. Homebrew formulae are split into
`harness`, `harness-model-proxy`, `harness-mcp-proxy`, and the `harness-full`
meta formula. The workflow then updates `ClarifiedLabs/homebrew-tap` through a
GitHub App installation token. After publishing the release assets and
container images, it also updates the generated release-artifact block in
`README.md` on the default branch and commits the versioned package links and
container tags. Rerunning an older release does not replace links for a newer
latest release.

After the GitHub release is published, the workflow adds the new `.deb` and
`.rpm` packages to signed APT and RPM repositories hosted in
`ClarifiedLabs/linux-packages` and served by GitHub Pages at
`https://clarifiedlabs.github.io/linux-packages`. Like the Homebrew tap update,
the package repository commit is pushed through a dedicated GitHub App
installation token.

### Linux package repositories

`ClarifiedLabs/linux-packages` is a plain GitHub Pages site ("Deploy from a
branch", `main` root) with this layout:

```
linux-packages/
  .nojekyll
  README.md
  harness-archive-keyring.asc      # public signing key
  harness.repo                     # static dnf/yum repo definition
  deb/                             # reprepro base: conf/, db/, dists/, pool/
  rpm/x86_64/                      # *.rpm + repodata/
  rpm/aarch64/                     # *.rpm + repodata/
```

- The APT repository is managed with `reprepro` and serves distribution
  `stable`, component `main`, architectures `amd64` and `arm64`. reprepro signs
  `InRelease`/`Release` on every update; the full reprepro tree (including
  `db/`) is committed so each release is an incremental add.
- The RPM repository is managed with `createrepo_c` at `rpm/$basearch`
  (`x86_64`, `aarch64`). `repodata/` is regenerated for each arch directory
  that received new packages, and `repomd.xml` is detached-signed
  (`repomd.xml.asc`).
- One passphrase-less RSA-4096 GPG key, `Clarified Labs, Inc. Packages
  <hello@clarified.io>`, signs the APT metadata, the RPM packages, and
  `repomd.xml`. The public half is served as `harness-archive-keyring.asc` and
  referenced by `gpgkey=` in `harness.repo`.

Both update scripts (`scripts/release/apt-repo-update.sh`,
`scripts/release/rpm-repo-update.sh`) are idempotent: re-running the publish
job for the same tag skips already-present packages and produces no new commit.
A rebuilt tag with the same version but different content fails loudly
(reprepro rejects the deb; the RPM script refuses to replace a differing file)
rather than silently replacing signed artifacts.

Operational notes:

- GitHub Pages soft caps apply (about 1 GB per site; the package pool grows
  roughly 40-50 MB per release), and Pages deployments lag the push by a
  minute or two.
- Old package versions are retained so users can pin or downgrade. Pruning old
  versions is manual for now (`reprepro remove` for debs; delete the files and
  re-run `createrepo_c` for rpms); keep-last-N retention is a deliberate
  follow-up.

Tarballs and the macOS `.pkg` include all three binaries. Homebrew formulae,
`.deb` packages, `.rpm` packages, and container images are split by binary:
`harness`, `harness-model-proxy`, and `harness-mcp-proxy`.

Container images are published to:

- `ghcr.io/clarifiedlabs/harness`
- `ghcr.io/clarifiedlabs/harness-model-proxy`
- `ghcr.io/clarifiedlabs/harness-mcp-proxy`

Image tags use the release version without the leading `v` plus `latest`, for
example `1.2.3` and `latest`. The `harness` image includes `git`,
`openssh-client`, and `ripgrep`; proxy images are minimal Debian Trixie images
with `ca-certificates` and the selected binary.

Proxy containers must bind wildcard addresses when exposing ports:

```sh
docker run --rm -p 8765:8765 ghcr.io/clarifiedlabs/harness-model-proxy:latest serve -listen 0.0.0.0:8765
docker run --rm -p 8766:8766 ghcr.io/clarifiedlabs/harness-mcp-proxy:latest serve -listen 0.0.0.0:8766
```

The tap repository must already exist with an initialized default branch. No
formula file is required ahead of time; the release workflow writes the formula
files and merges the generated bottle metadata. Go builds in the binary formulae
set `GOWORK=off` so unrelated workspaces (including a `go.work` in a parent
temporary directory) cannot interfere with source builds.

## CI Dry Runs

Push a branch named `release-ci` or under `release-ci/`, or run the `release`
workflow manually, to exercise the release workflow without publishing. Dry-run
builds use version `v0.0.0` and the pushed commit archive as the Homebrew source.
They build and upload the normal workflow artifacts, build container images
without pushing them, generate checksums, build Homebrew bottles from a local
tap, and dry-run the Homebrew formula merge.

Dry runs do not publish a GitHub release, push container images, push to the
Homebrew tap, push to the package repositories, or create artifact
attestations. The macOS `.pkg` is built unsigned in dry runs so Apple Developer
ID and notarization secrets are only required for real `v*` tag releases. They
render the README release-artifact block with `v0.0.0` and show its diff
without committing it.

The `packages-publish-dry-run` job exercises the package repository pipeline
end to end without secrets: it generates a throwaway GPG key, builds scratch
APT and RPM repositories from the dry-run `.deb` and (unsigned) `.rpm`
packages with the same update scripts the real publish uses, and verifies the
`InRelease` and `repomd.xml` signatures and the expected six-package set per
format. Nothing is pushed.

## Tagging

Create release tags with:

```sh
make release VERSION=patch
make release VERSION=minor
make release VERSION=major
make release VERSION=1.2.3
make release VERSION=patch AUTOPUSH=1
```

`patch`, `minor`, and `major` are computed from the latest `vX.Y.Z` git tag.
`patch` starts at `v0.0.1` when no prior tag exists. The target requires a clean
worktree, runs `go build ./...`, `go vet ./...`, and `go test ./...`, then
creates an annotated `vX.Y.Z` tag. `AUTOPUSH=1` pushes the tag to `origin`.

## Required Secrets And Variables

- `MACOS_DEVELOPER_ID_APPLICATION_P12_BASE64`: base64 of a `.p12` exported from
  Certificates, Identifiers & Profiles -> Certificates -> **Developer ID
  Application**. Export it with the private key from Keychain Access.
- `MACOS_DEVELOPER_ID_APPLICATION_PASSWORD`: password used when exporting that
  Application `.p12`.
- `MACOS_DEVELOPER_ID_INSTALLER_P12_BASE64`: base64 of a `.p12` exported from
  Certificates, Identifiers & Profiles -> Certificates -> **Developer ID
  Installer**.
- `MACOS_DEVELOPER_ID_INSTALLER_PASSWORD`: password used when exporting that
  Installer `.p12`.
- `APPLE_TEAM_ID`: the Apple Developer Team ID, visible in the developer account
  membership page and in Developer ID certificate subjects.
- `APPLE_NOTARY_KEY_ID`, `APPLE_NOTARY_ISSUER_ID`,
  `APPLE_NOTARY_KEY_P8_BASE64`: an **App Store Connect API key** for
  notarization. This is created in App Store Connect under Users and Access ->
  Integrations -> App Store Connect API, not in the
  Certificates/Identifiers/Profiles certificate list. Download the `.p8` key
  once and base64 it for the secret.
- `MACOS_DEVELOPER_ID_APPLICATION_IDENTITY` and
  `MACOS_DEVELOPER_ID_INSTALLER_IDENTITY` are optional override secrets for the
  exact certificate common names if automatic identity discovery is ambiguous.
- `HOMEBREW_TAP_APP_PRIVATE_KEY`: private key for the GitHub App installed on
  `ClarifiedLabs/homebrew-tap`.
- `HOMEBREW_TAP_APP_CLIENT_ID`: the GitHub App Client ID.
- `PACKAGES_GPG_PRIVATE_KEY`: ASCII-armored private half of the `Clarified
  Labs, Inc. Packages <hello@clarified.io>` GPG key. Signs the APT
  `InRelease`/`Release` metadata, the RPM packages, and `repomd.xml`.
- `PACKAGES_APP_PRIVATE_KEY`: private key for the GitHub App installed on
  `ClarifiedLabs/linux-packages`.
- `PACKAGES_APP_CLIENT_ID`: that GitHub App's Client ID.

Each GitHub App only needs to be installed on its target repository
(`ClarifiedLabs/homebrew-tap` or `ClarifiedLabs/linux-packages`) with
repository Contents read/write permission. No Apple provisioning profile is
used for this Developer ID CLI/pkg distribution flow.

### Linux package repository setup (one time)

1. Create the public repository `ClarifiedLabs/linux-packages` with an initial
   commit containing:
   - `.nojekyll` (empty; disables Jekyll processing on Pages)
   - `README.md` with the apt/dnf install snippets from the main README
   - `deb/conf/distributions`:
     ```
     Origin: Clarified Labs, Inc.
     Label: harness
     Suite: stable
     Codename: stable
     Components: main
     Architectures: amd64 arm64
     SignWith: yes
     ```
   - `harness.repo` (note the literal `$basearch`):
     ```ini
     [harness]
     name=Clarified Labs, Inc. harness
     baseurl=https://clarifiedlabs.github.io/linux-packages/rpm/$basearch
     enabled=1
     gpgcheck=1
     repo_gpgcheck=1
     gpgkey=https://clarifiedlabs.github.io/linux-packages/harness-archive-keyring.asc
     ```
   - `harness-archive-keyring.asc` (public key from step 2)
2. Generate the signing key:
   ```sh
   gpg --batch --gen-key <<'EOF'
   Key-Type: RSA
   Key-Length: 4096
   Name-Real: Clarified Labs, Inc. Packages
   Name-Email: hello@clarified.io
   Expire-Date: 0
   %no-protection
   %commit
   EOF
   ```
   Export the public half with `gpg --armor --export hello@clarified.io >
   harness-archive-keyring.asc` and commit it to `linux-packages`; store the
   secret half (`gpg --armor --export-secret-keys hello@clarified.io`) as the
   `PACKAGES_GPG_PRIVATE_KEY` secret on `ClarifiedLabs/harness`.
3. Create a GitHub App with Contents read/write permission, install it on
   `ClarifiedLabs/linux-packages` only, and add the `PACKAGES_APP_CLIENT_ID`
   and `PACKAGES_APP_PRIVATE_KEY` secrets to `ClarifiedLabs/harness`.
4. In `linux-packages` → Settings → Pages, choose "Deploy from a branch",
   branch `main`, `/ (root)`.
