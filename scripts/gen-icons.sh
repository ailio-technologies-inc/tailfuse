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

# Menu-bar animation while tailnets connect: a spark runs round the dots (the
# fuse burning), with a fading tail; the bar in the middle stays lit. Frames
# tray-connecting-0..9.png, played by lib.rs (spawn_tray_animation).
PATH_DOTS=(150,176 214,176 298,176 362,176 362,256 362,336 298,336 214,336 150,336 150,256)
N=${#PATH_DOTS[@]}
FR="$ROOT/app/src-tauri/icons/tray-connecting"
mkdir -p "$FR"
for ((f = 0; f < N; f++)); do
  svg='<svg xmlns="http://www.w3.org/2000/svg" viewBox="126 152 260 208">'
  for ((i = 0; i < N; i++)); do
    d=$(((f - i + N) % N))
    case $d in 0) c='#ffb03a'; r=20 ;; 1) c='#ff4a1f'; r=17 ;; 2) c='#c2462a'; r=17 ;; *) c='#8e9096'; r=17 ;; esac
    svg+="<circle cx=\"${PATH_DOTS[$i]%,*}\" cy=\"${PATH_DOTS[$i]#*,}\" r=\"$r\" fill=\"$c\"/>"
  done
  svg+='<rect x="214" y="239" width="84" height="34" rx="17" fill="#ff4a1f"/></svg>'
  printf '%s' "$svg" | rsvg-convert -h 36 -o "$FR/$f.png"
done

# Site.
P="$ROOT/site/public"
cp "$B/favicon.svg" "$P/favicon.svg"
rsvg-convert -w 48 "$B/favicon.svg" -o "$P/favicon-48.png"
rsvg-convert -w 180 "$B/app-icon-square.svg" -o "$P/apple-touch-icon.png"
rsvg-convert -w 192 "$B/app-icon.svg" -o "$P/icon-192.png"
rsvg-convert -w 512 "$B/app-icon.svg" -o "$P/icon-512.png"
echo "gen-icons: app, menu-bar and site icons regenerated from brand/"
