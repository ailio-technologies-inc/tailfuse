#!/usr/bin/env bash
# Builds the Mac tunnel (all-apps mode, src-tauri/src/allapps.rs) into a built Tailfuse.app and
# signs both with the provisioning profiles NetworkExtension requires.
#
#   apps/desktop/macos/build-tunnel.sh path/to/Tailfuse.app [--notarize | --assemble-only]
#
# --assemble-only builds the extension into the bundle and stops: no profiles, no signing, and the
# team ID left as __TEAM_ID__. The release build on the Mac mini (infra host/tailfuse-release-macos)
# runs it, and the cluster's notarize step (rcodesign, on Linux) fills in the team ID, adds the
# profiles and signs with the entitlements here.
#
# Needs: Xcode's command line tools; the engine framework (engines/mobile/scripts/build-engine.sh
# macos, run here when missing); and, to sign, your Apple account's details, kept out of the repo:
#   APPLE_SIGNING_IDENTITY  the Developer ID Application identity in the keychain,
#                           e.g. "Developer ID Application: Your Name (TEAMID1234)"
#   APPLE_TEAM_ID      the 10-character team ID, filled in for __TEAM_ID__ in the entitlements
#                      and the extension's Info.plist
#   TF_APP_PROFILE     com.ailio.tailfuse          (Network Extensions + System Extension)
#   TF_TUNNEL_PROFILE  com.ailio.tailfuse.tunnel   (Network Extensions)
# --notarize submits the signed app with the App Store Connect key in ~/.appstoreconnect, as
# scripts/notarize-macos.sh does. macOS loads a Developer ID system extension only once notarized.
set -euo pipefail

app="${1:?usage: $0 path/to/Tailfuse.app [--notarize | --assemble-only]}"
mode="${2:-}"
notarize=""
[ "$mode" = "--notarize" ] && notarize="--notarize"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
if [ "$mode" != "--assemble-only" ]; then
	: "${APPLE_SIGNING_IDENTITY:?set APPLE_SIGNING_IDENTITY to your Developer ID Application identity}"
	: "${APPLE_TEAM_ID:?set APPLE_TEAM_ID to your 10-character Apple team ID}"
	: "${TF_APP_PROFILE:?set TF_APP_PROFILE to the com.ailio.tailfuse Developer ID profile}"
	: "${TF_TUNNEL_PROFILE:?set TF_TUNNEL_PROFILE to the com.ailio.tailfuse.tunnel Developer ID profile}"
fi
[ -d "$app/Contents" ] || { echo "$app is not an app bundle" >&2; exit 1; }

fw="$here/Frameworks/Tfmobile.xcframework/macos-arm64_x86_64"
[ -d "$fw/Tfmobile.framework" ] || "$root/engines/mobile/scripts/build-engine.sh" macos

version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")"
work="$root/target/macos-tunnel"
rm -rf "$work"
mkdir -p "$work"

for arch in arm64 x86_64; do
	xcrun swiftc -O -module-name TailfuseTunnel -target "$arch-apple-macos13.0" \
		-F "$fw" -framework Tfmobile -framework NetworkExtension -lresolv \
		-o "$work/tunnel-$arch" "$here/TailfuseTunnel/main.swift" "$here/TailfuseTunnel/PacketTunnelProvider.swift"
done

ext="$app/Contents/Library/SystemExtensions/com.ailio.tailfuse.tunnel.systemextension"
rm -rf "$ext"
mkdir -p "$ext/Contents/MacOS"
lipo -create -output "$ext/Contents/MacOS/com.ailio.tailfuse.tunnel" "$work/tunnel-arm64" "$work/tunnel-x86_64"
sed "s/__VERSION__/$version/g" "$here/TailfuseTunnel/Info.plist" >"$ext/Contents/Info.plist"
if [ "$mode" = "--assemble-only" ]; then
	echo "assembled the tunnel into $app ($version); not signed, team ID left as __TEAM_ID__"
	exit 0
fi
identity="$APPLE_SIGNING_IDENTITY"
sed -i '' "s/__TEAM_ID__/$APPLE_TEAM_ID/g" "$ext/Contents/Info.plist"
sed "s/__TEAM_ID__/$APPLE_TEAM_ID/g" "$here/TailfuseTunnel/TailfuseTunnel.entitlements" >"$work/tunnel.entitlements"
sed "s/__TEAM_ID__/$APPLE_TEAM_ID/g" "$here/Tailfuse.entitlements" >"$work/app.entitlements"
cp "$TF_TUNNEL_PROFILE" "$ext/Contents/embedded.provisionprofile"
cp "$TF_APP_PROFILE" "$app/Contents/embedded.provisionprofile"

# Inside out: the extension, the sidecars, then the app (which seals everything in it).
codesign --force --timestamp --options runtime --sign "$identity" \
	--entitlements "$work/tunnel.entitlements" "$ext"
for bin in "$app"/Contents/MacOS/*; do
	[ "$(basename "$bin")" = "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$app/Contents/Info.plist")" ] && continue
	codesign --force --timestamp --options runtime --sign "$identity" "$bin"
done
codesign --force --timestamp --options runtime --sign "$identity" --entitlements "$work/app.entitlements" "$app"
codesign --verify --deep --strict --verbose=2 "$app"

if [ "$notarize" = "--notarize" ]; then
	env_file="$(ls "$HOME"/.appstoreconnect/*.env | head -1)"
	# shellcheck disable=SC1090
	. "$env_file"
	zip="$work/Tailfuse-notarize.zip"
	ditto -c -k --keepParent "$app" "$zip"
	xcrun notarytool submit "$zip" --key "$HOME/.appstoreconnect/private_keys/AuthKey_$APPLE_API_KEY.p8" \
		--key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait
	xcrun stapler staple "$app"
fi
echo "signed $app with its tunnel ($version)"
