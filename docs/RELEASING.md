# Releasing Tailfuse

```sh
scripts/release.sh 0.1.16
```

That bumps the version everywhere (the Cargo workspace, `apps/desktop/src-tauri/tauri.conf.json`, `apps/desktop/package.json`), runs the tests, commits, tags `v0.1.16` and pushes. The tag starts everything else; nothing is built on a laptop.

Every desktop build runs `apps/desktop/scripts/stage-tailscale.mjs`. It builds the pinned `tailscale` and `tailscaled` sidecars from Tailscale's checksum-verified Go module (including a universal binary for macOS) and Tauri bundles them beside the app executable. App builders therefore need the Go version pinned in `mise.toml`; end users do not need Tailscale installed.

## What happens after the tag

| Where | What |
|---|---|
| GitHub Actions (`.github/workflows/release.yml`) | Builds only the Linux menu-bar app (AppImage and .deb), signed with the updater key (`TAURI_SIGNING_PRIVATE_KEY`, a repository secret), and creates the release if no other lane has yet. |
| The Ailio cluster (Dagu `tailfuse-release`, queued by the tag's webhook) | Builds the CLI archives for every platform. `tailfuse-release-windows` builds the Windows installer with its updater `.sig`. `tailfuse-release-macos`, on the Mac mini, builds the universal app with the Every app tunnel assembled in, unsigned (`apps/desktop/macos/build-tunnel.sh --assemble-only`). Then `tailfuse-notarize`, on Linux with rcodesign: embeds the app's and the tunnel's Developer ID provisioning profiles, signs the extension, app and CLI with the Developer ID, notarizes and staples them, publishes `…-macos-universal.zip` and the notarized updater bundle `…-macos-universal.app.tar.gz` with its `.sig`, writes `latest.json` from every platform's signature, and moves the Homebrew cask to the new version. |
| tailfuse.com | Nothing to deploy: `/download` redirects to the latest release's files. |

Not signed yet: the Windows installer has no Authenticode signature, so SmartScreen warns on first run (updates are still verified with the updater key). The phone apps have no release lane.

The credentials stay out of this repository: the Apple key and certificate and the updater key live on the owner's laptop and in the cluster's secret store (infra: `bake-host/signing-secrets.json`).

## Automatic updates

The app reads `https://github.com/ailio-technologies-inc/tailfuse/releases/latest/download/latest.json` a minute after launch and every six hours (`apps/desktop/src-tauri/src/updates.rs`). An update installs in the background and the app relaunches into it once Tailfuse is off. Each download is checked against the updater public key in `tauri.conf.json`; a file not signed with the private key is refused.

Check a release's feed:

```sh
curl -sL https://github.com/ailio-technologies-inc/tailfuse/releases/latest/download/latest.json
```

It should name the new version and four platforms: `darwin-aarch64`, `darwin-x86_64`, `windows-x86_64`, `linux-x86_64`. A platform whose lane failed is left out, and its users update with the next release.

**Keep the updater key.** It is `~/.tauri/tailfuse-updater.key` (and its `.pub`). Losing it means installed copies can never verify another update, and everyone would have to reinstall by hand.

## Screenshots

`scripts/screenshots.sh` renders the real UI (`apps/desktop/src`, with the preview stub's `?shot` state) into `docs/screenshots/` and `apps/site/public/shots/`. Run it after a visible UI change, then deploy the site (`cd apps/site && npx wrangler deploy`).
