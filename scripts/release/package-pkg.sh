#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${GOARCH:?GOARCH is required}"

stage_dir="${STAGE_DIR:-dist/bin/darwin-${GOARCH}}"
dist_dir="${DIST_DIR:-dist}"
work_dir="${WORK_DIR:-dist/package-pkg}"
version="${VERSION#v}"
pkg="${dist_dir}/harness_${VERSION}_darwin_${GOARCH}.pkg"
signing_mode="${MACOS_PKG_SIGNING_MODE:-signed}"

case "$signing_mode" in
	signed | unsigned) ;;
	*) echo "unsupported MACOS_PKG_SIGNING_MODE: ${signing_mode}" >&2; exit 2 ;;
esac

if [[ "$signing_mode" == "signed" ]]; then
	: "${MACOS_DEVELOPER_ID_APPLICATION_P12_BASE64:?MACOS_DEVELOPER_ID_APPLICATION_P12_BASE64 is required}"
	: "${MACOS_DEVELOPER_ID_APPLICATION_PASSWORD:?MACOS_DEVELOPER_ID_APPLICATION_PASSWORD is required}"
	: "${MACOS_DEVELOPER_ID_INSTALLER_P12_BASE64:?MACOS_DEVELOPER_ID_INSTALLER_P12_BASE64 is required}"
	: "${MACOS_DEVELOPER_ID_INSTALLER_PASSWORD:?MACOS_DEVELOPER_ID_INSTALLER_PASSWORD is required}"
	: "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required}"
	: "${APPLE_NOTARY_KEY_ID:?APPLE_NOTARY_KEY_ID is required}"
	: "${APPLE_NOTARY_ISSUER_ID:?APPLE_NOTARY_ISSUER_ID is required}"
	: "${APPLE_NOTARY_KEY_P8_BASE64:?APPLE_NOTARY_KEY_P8_BASE64 is required}"
fi

rm -rf "$work_dir"
mkdir -p "$work_dir" "$dist_dir"

if [[ "$signing_mode" == "signed" ]]; then
	keychain="${work_dir}/release.keychain-db"
	keychain_password="$(openssl rand -hex 16)"
	security create-keychain -p "$keychain_password" "$keychain"
	security default-keychain -s "$keychain"
	security unlock-keychain -p "$keychain_password" "$keychain"
	security set-keychain-settings -lut 21600 "$keychain"

	printf '%s' "$MACOS_DEVELOPER_ID_APPLICATION_P12_BASE64" | base64 -D >"${work_dir}/application.p12"
	printf '%s' "$MACOS_DEVELOPER_ID_INSTALLER_P12_BASE64" | base64 -D >"${work_dir}/installer.p12"
	printf '%s' "$APPLE_NOTARY_KEY_P8_BASE64" | base64 -D >"${work_dir}/notary.p8"

	security import "${work_dir}/application.p12" -k "$keychain" -P "$MACOS_DEVELOPER_ID_APPLICATION_PASSWORD" -T /usr/bin/codesign
	security import "${work_dir}/installer.p12" -k "$keychain" -P "$MACOS_DEVELOPER_ID_INSTALLER_PASSWORD" -T /usr/bin/pkgbuild -T /usr/bin/productbuild
	security set-key-partition-list -S apple-tool:,apple: -s -k "$keychain_password" "$keychain"
fi

find_identity() {
	local kind="$1"
	security find-identity -v -p basic "$keychain" |
		awk -v kind="$kind" -v team="$APPLE_TEAM_ID" 'index($0, kind) && index($0, "(" team ")") { sub(/^[^"]*"/, ""); sub(/".*$/, ""); print; exit }'
}

if [[ "$signing_mode" == "signed" ]]; then
	app_identity="${MACOS_DEVELOPER_ID_APPLICATION_IDENTITY:-$(find_identity "Developer ID Application")}"
	installer_identity="${MACOS_DEVELOPER_ID_INSTALLER_IDENTITY:-$(find_identity "Developer ID Installer")}"
	if [[ -z "$app_identity" ]]; then
		echo "Developer ID Application certificate for team ${APPLE_TEAM_ID} was not found" >&2
		exit 1
	fi
	if [[ -z "$installer_identity" ]]; then
		echo "Developer ID Installer certificate for team ${APPLE_TEAM_ID} was not found" >&2
		exit 1
	fi
fi

payload="${work_dir}/payload"
mkdir -p "${payload}/usr/local/bin"
for name in harness harness-model-proxy harness-mcp-proxy; do
	install -m 0755 "${stage_dir}/${name}" "${payload}/usr/local/bin/${name}"
	if [[ "$signing_mode" == "signed" ]]; then
		codesign --force --timestamp --options runtime --sign "$app_identity" "${payload}/usr/local/bin/${name}"
		codesign --verify --strict --verbose=2 "${payload}/usr/local/bin/${name}"
	fi
done

pkgbuild_args=(
	--root "$payload" \
	--identifier "com.clarifiedlabs.harness" \
	--version "$version" \
	--install-location "/"
)
if [[ "$signing_mode" == "signed" ]]; then
	pkgbuild_args+=(--sign "$installer_identity")
fi
pkgbuild "${pkgbuild_args[@]}" "$pkg"

if [[ "$signing_mode" == "unsigned" ]]; then
	pkgutil --expand "$pkg" "${work_dir}/expanded"
	exit 0
fi

xcrun notarytool submit "$pkg" \
	--key "${work_dir}/notary.p8" \
	--key-id "$APPLE_NOTARY_KEY_ID" \
	--issuer "$APPLE_NOTARY_ISSUER_ID" \
	--wait
xcrun stapler staple "$pkg"
pkgutil --check-signature "$pkg"
spctl -a -vv -t install "$pkg"
