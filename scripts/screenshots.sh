#!/usr/bin/env bash
# Screenshots of the real app UI for the README (docs/screenshots/) and tailfuse.com (site/public/shots/).
# The app's own index.html and panel.html are rendered in headless Chrome with the preview stub's ?shot state
# (app/src/preview-stub.js: three tailnets, licensed, no notices), inside a macOS-style window frame, at 2x.
#
#   scripts/screenshots.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
[ -x "$CHROME" ] || { echo "screenshots: no Chrome at $CHROME (set CHROME)" >&2; exit 1; }
PORT=${PORT:-8797}

WORK=$(mktemp -d)
trap 'kill "$SERVER" 2>/dev/null || true; rm -rf "$WORK"' EXIT
cp -R "$ROOT/app/src" "$WORK/www"

# frame.html?page=index.html&w=720&h=820: the page in an iframe, as a window with traffic lights and a shadow.
cat > "$WORK/www/frame.html" <<'HTML'
<!doctype html>
<meta charset="utf-8">
<style>
  :root { color-scheme: dark; }
  html, body { margin: 0; background: transparent; }
  body { padding: 56px 64px 80px; display: inline-block; }
  .win { position: relative; border-radius: 12px; overflow: hidden; background: #1c1d20;
         box-shadow: 0 0 0 1px rgba(255,255,255,.10), 0 0 0 1.5px rgba(0,0,0,.6), 0 28px 70px rgba(0,0,0,.45), 0 8px 22px rgba(0,0,0,.3); }
  .win.panel { background: transparent; box-shadow: none; overflow: visible; }
  .win.panel iframe { filter: drop-shadow(0 24px 40px rgba(0,0,0,.45)) drop-shadow(0 6px 14px rgba(0,0,0,.3)); }
  iframe { display: block; border: 0; }
  .lights { position: absolute; top: 9px; left: 12px; display: flex; gap: 8px; pointer-events: none; }
  .lights i { width: 12px; height: 12px; border-radius: 50%; box-shadow: inset 0 0 0 .5px rgba(0,0,0,.25); }
  .lights i:nth-child(1) { background: #ff5f57; } .lights i:nth-child(2) { background: #febc2e; } .lights i:nth-child(3) { background: #28c840; }
</style>
<div class="win" id="win"><iframe id="f"></iframe><div class="lights" id="lights"><i></i><i></i><i></i></div></div>
<script>
  const q = new URLSearchParams(location.search);
  const f = document.getElementById("f");
  f.width = q.get("w"); f.height = q.get("h");
  f.src = q.get("page") + "?shot" + (q.get("at") ? "#" + q.get("at") : "");
  if (q.has("panel")) { document.getElementById("win").classList.add("panel"); document.getElementById("lights").remove(); }
</script>
HTML

(cd "$WORK/www" && exec python3 -m http.server "$PORT" --bind 127.0.0.1 >/dev/null 2>&1) &
SERVER=$!
sleep 1

shoot() { # shoot <out.png> <frame query> <viewport w> <viewport h>
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=2 \
    --default-background-color=00000000 --user-data-dir="$WORK/chrome" --virtual-time-budget=4000 \
    --window-size="$3,$4" --screenshot="$1" "http://127.0.0.1:$PORT/frame.html?$2" 2>/dev/null
}
mkdir -p "$ROOT/docs/screenshots" "$ROOT/site/public/shots"
shoot "$ROOT/docs/screenshots/app.png" "page=index.html&w=720&h=820" 848 956
shoot "$ROOT/docs/screenshots/settings.png" "page=index.html&w=720&h=640&at=license-h" 848 776
shoot "$ROOT/docs/screenshots/panel.png" "page=panel.html&w=340&h=318&panel" 468 454
cp "$ROOT/docs/screenshots/"{app,settings,panel}.png "$ROOT/site/public/shots/"
echo "screenshots: docs/screenshots/{app,settings,panel}.png and site/public/shots/"
