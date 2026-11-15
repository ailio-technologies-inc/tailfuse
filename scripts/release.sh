#!/usr/bin/env bash
# Cut a release: set the version everywhere, commit, tag and push. The tag does the rest
# (docs/RELEASING.md): GitHub Actions builds macOS, Windows and Linux; the cluster builds the Linux
# CLIs, signs and notarizes macOS, and moves the Homebrew cask; tailfuse.com/download serves it.
#
#   scripts/release.sh 0.1.15
set -euo pipefail
V="${1:-}"
[[ "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "usage: scripts/release.sh <x.y.z>" >&2; exit 2; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || { echo "release: not on main" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "release: the working tree has changes; commit or stash them first" >&2; exit 1; }
git fetch -q origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "release: main is not up to date with origin" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/v$V" >/dev/null && { echo "release: v$V already exists" >&2; exit 1; }

# One version for the Rust workspace (the CLI's --version), the app and its npm package.
node -e 'const fs=require("fs");const s=fs.readFileSync("Cargo.toml","utf8");fs.writeFileSync("Cargo.toml",s.replace(/^version = "[^"]+"/m,`version = "${process.argv[1]}"`))' "$V"
node -e '
  const fs=require("fs"); const v=process.argv[1];
  for (const f of ["apps/desktop/src-tauri/tauri.conf.json","apps/desktop/package.json"]) {
    const j=JSON.parse(fs.readFileSync(f,"utf8")); j.version=v; fs.writeFileSync(f, JSON.stringify(j,null,2)+"\n");
  }' "$V"
cargo update -q --workspace
cargo test -q -p tailfuse-core -p tailfuse-cli >/dev/null

git add Cargo.toml Cargo.lock apps/desktop/src-tauri/tauri.conf.json apps/desktop/package.json
git commit -q -m "Release v$V"
git tag "v$V"
git push -q origin main "v$V"
echo "release: v$V pushed. Watch: gh run list --workflow release.yml; the cluster's tailfuse-release DAG notarizes macOS."
