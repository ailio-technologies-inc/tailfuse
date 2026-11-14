#!/usr/bin/env bash
# Notarize + staple the built macOS bundle, the same flow dagpeek uses
# (scripts/release.sh there; ~/.appstoreconnect holds the App Store Connect
# key and issuer, out of any repo). Run after 'npm run tauri build'.
#
#   scripts/notarize-macos.sh            # the bundle in target/release
#   scripts/notarize-macos.sh <dmg>      # a specific dmg
set -euo pipefail

IDENTITY="Developer ID Application: Luigi Poole (N7TM93NL4Q)"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DMG="${1:-$(ls "$ROOT"/target/release/bundle/dmg/*.dmg 2>/dev/null | head -1)}"
[ -f "$DMG" ] || { echo "no dmg found (build first, or pass its path)" >&2; exit 1; }

# The App Store Connect key: issuer + key id from ~/.appstoreconnect/*.env,
# the .p8 from ~/.appstoreconnect/private_keys/ (mirrors dagpeek's layout).
ENV_FILE="$(ls "$HOME"/.appstoreconnect/*.env 2>/dev/null | head -1)"
[ -f "$ENV_FILE" ] || { echo "no ~/.appstoreconnect/*.env (issuer + key id)" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"
: "${APPLE_API_ISSUER:?APPLE_API_ISSUER missing in $ENV_FILE}"
: "${APPLE_API_KEY:?APPLE_API_KEY missing in $ENV_FILE}"
KEY="$HOME/.appstoreconnect/private_keys/AuthKey_$APPLE_API_KEY.p8"
[ -f "$KEY" ] || { echo "no $KEY" >&2; exit 1; }

echo "== signing $DMG"
codesign --force --sign "$IDENTITY" --timestamp "$DMG"

echo "== submitting to Apple"
xcrun notarytool submit "$DMG" --key "$KEY" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait

echo "== stapling"
xcrun stapler staple "$DMG"
APP="$ROOT/target/release/bundle/macos/tailfuse.app"
[ -d "$APP" ] && xcrun stapler staple "$APP" || true

echo "== verifying"
spctl -a -vv "$APP" || true
echo "done: $DMG is notarized and stapled"
