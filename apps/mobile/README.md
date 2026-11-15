# Tailfuse mobile app

The Expo (React Native, TypeScript) interface for Tailfuse on iPhone and Android. It mirrors the
desktop window in `app/src`: same palette, type, wording and section order.

## Preview in a browser

```sh
npm install
npx expo start --web
```

With no native engine linked (web, Expo Go) the app runs against an in-memory mock
(`src/mock.ts`) that behaves like the engine: two tailnets and a local port to start with, a
sign-in that "completes" a few seconds after you add a tailnet, working switches and removal, and
a pretend purchase for "Unlimited tailnets".

## The real engine

The Go engine, the VPN shells and in-app purchases are native, so they need a development build
(`expo prebuild` / EAS), not Expo Go. The contract is `../../engines/mobile/API.md`.

## iOS device build

The app ships with a packet tunnel extension (`targets/tunnel`, added to the Xcode project by
`@bacons/apple-targets`) that runs the engine. The simulator can build both but can't run a
packet tunnel, so try it on a phone.

On the Apple developer account, before the first signed build:

- App IDs `com.ailio.tailfuse` and `com.ailio.tailfuse.tunnel`, each with the **Network
  Extensions** capability (packet tunnel) and the **App Groups** capability set to
  `group.com.ailio.tailfuse` (create the group too).

Then:

```sh
../../engines/mobile/scripts/build-engine.sh ios   # the Go framework (gitignored)
# add "appleTeamId": "<your 10-character team id>" under expo.ios in app.json (signs both targets)
npx expo prebuild --platform ios --clean
npx expo run:ios --device --configuration Release  # a phone on USB, Xcode signing
# or in the cloud: npx eas-cli@latest build --platform ios (EAS makes profiles for both bundle ids)
```

`modules/tailfuse-engine/ios/README.md` explains how the pieces fit and what to check on the phone.

## Where things live

- `App.tsx`, `src/` — the screen. `src/logic.ts` holds the pure rules (status line, row state,
  friendly errors, the free limit); test them with `npm test`.
- `src/engine.ts` — typed wrapper over the engine (`getState`, `addProfile`, ...). `src/store.ts`
  — the one-time purchase (`com.ailio.tailfuse.unlimited`, via `expo-iap`).
- `modules/tailfuse-engine/` — the local Expo module: the TypeScript surface, the Swift side in
  `ios/` and the Kotlin side in `android/`. `targets/tunnel/` — the iOS packet tunnel extension.
- `scripts/gen-assets.ts` — regenerates the icons and splash from `../../brand` (`npx tsx scripts/gen-assets.ts`).
