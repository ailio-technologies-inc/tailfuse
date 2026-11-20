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
export APPLE_TEAM_ID=<your 10-character team id>  # or put it in .env.local; app.config.ts signs both targets with it
npx expo prebuild --platform ios --clean
npx expo run:ios --device --configuration Release  # a phone on USB, Xcode signing
# or in the cloud: npx eas-cli@latest build --platform ios (EAS makes profiles for both bundle ids)
```

For TestFlight, bump `expo.ios.buildNumber` in `app.json` (App Store Connect refuses a number it
has seen), archive in Xcode or with `xcodebuild archive`, then export the `.ipa` and upload it with
Transporter or `xcrun altool --upload-app`:

```sh
xcodebuild -exportArchive -archivePath build/Tailfuse.xcarchive \
  -exportOptionsPlist scripts/ExportOptions.plist -exportPath build/export
```

`app.json` does not set `ITSAppUsesNonExemptEncryption`: the tunnel uses WireGuard's encryption,
so App Store Connect asks the export-compliance question for each build.

`modules/tailfuse-engine/ios/README.md` explains how the pieces fit and what to check on the phone.

## Android build

`mise install` (repo root) gives you JDK 17 (Temurin), and `mise.toml` points `ANDROID_HOME`, `ANDROID_SDK_ROOT` and `ANDROID_NDK_HOME` at `~/Library/Android/sdk` and puts `sdkmanager` and `adb` on the PATH. It does not install the Android SDK itself. Once, download Google's `commandlinetools-mac` zip from https://developer.android.com/studio#command-line-tools-only into `~/Library/Android/sdk/cmdline-tools/latest`, then:

```sh
yes | sdkmanager --licenses
sdkmanager "platform-tools" "platforms;android-37.0" "build-tools;37.0.0" "ndk;27.1.12297006" "cmake;3.30.5"
```

These are the versions React Native 0.88 asks for (compileSdk 37, build-tools 37.0.0, NDK 27.1.12297006, CMake 3.30.5, Gradle 9.4.1 through the wrapper). The one NDK also serves gomobile (`-androidapi 26`). Then:

```sh
moon run mobile-engine:bind-android                 # the Go .aar (gitignored); TF_ANDROID_ABIS=arm64 builds one ABI
npx expo prebuild --platform android
cd android && ./gradlew :app:assembleDebug -PreactNativeArchitectures=arm64-v8a
```

Built and checked on an Apple Silicon Mac (arm64 debug APK about 90 MB, `lib/arm64-v8a/libgojni.so` inside, merged manifest has the VPN service and permissions). Not run on a device or emulator. Give it about 8 GB of free disk and 7 minutes for the first Gradle build (it downloads Gradle 9.4.1 and the Android Gradle plugin); `~/.gradle` grows to about 4 GB. The APK is `android/app/build/outputs/apk/debug/app-debug.apk`; install it with `adb install -r`.

## Where things live

- `App.tsx`, `src/` — the screen. `src/logic.ts` holds the pure rules (status line, row state,
  friendly errors, the free limit); test them with `npm test`.
- `src/engine.ts` — typed wrapper over the engine (`getState`, `addProfile`, ...). `src/store.ts`
  — the one-time purchase (`com.ailio.tailfuse.unlimited`, via `expo-iap`).
- `modules/tailfuse-engine/` — the local Expo module: the TypeScript surface, the Swift side in
  `ios/` and the Kotlin side in `android/`. `targets/tunnel/` — the iOS packet tunnel extension.
- `scripts/gen-assets.ts` — regenerates the icons and splash from `../../brand` (`npx tsx scripts/gen-assets.ts`).
