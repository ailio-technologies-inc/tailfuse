#!/usr/bin/env bash
# Regenerate every icon from the sources in brand/ (the fused-dots mark):
#   the app icon set (app/src-tauri/icons, via `tauri icon`), the menu-bar glyph,
#   and the site's favicon, touch and manifest icons.
# Needs rsvg-convert (brew install librsvg) and the app's npm packages (cd app && npm ci).
#
#   scripts/gen-icons.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
B="$ROOT/brand"
command -v rsvg-convert >/dev/null || { echo "gen-icons: needs rsvg-convert (brew install librsvg)" >&2; exit 1; }

# App icon set (all sizes, .icns, .ico) from the 1024 tile.
(cd "$ROOT/app" && npx tauri icon "$B/app-icon.svg" >/dev/null)
rm -rf "$ROOT/app/src-tauri/icons/android" "$ROOT/app/src-tauri/icons/ios"

# Menu-bar glyph, 1x and 2x.
rsvg-convert -h 18 "$B/tray.svg" -o "$ROOT/app/src-tauri/icons/tray-18.png"
rsvg-convert -h 36 "$B/tray.svg" -o "$ROOT/app/src-tauri/icons/tray-36.png"

# Site.
P="$ROOT/site/public"
cp "$B/favicon.svg" "$P/favicon.svg"
rsvg-convert -w 48 "$B/favicon.svg" -o "$P/favicon-48.png"
rsvg-convert -w 180 "$B/app-icon-square.svg" -o "$P/apple-touch-icon.png"
rsvg-convert -w 192 "$B/app-icon.svg" -o "$P/icon-192.png"
rsvg-convert -w 512 "$B/app-icon.svg" -o "$P/icon-512.png"
echo "gen-icons: app, menu-bar and site icons regenerated from brand/"
