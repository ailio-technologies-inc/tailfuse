#!/usr/bin/env bash
# Build the Tailfuse mobile engine bindings with gomobile.
#
#   engines/mobile/scripts/build-engine.sh ios|android|all
#
# ios:     apps/mobile/modules/tailfuse-engine/ios/Frameworks/Tfmobile.xcframework
#          (device arm64 + simulator arm64/x86_64, minimum iOS 16)
# android: apps/mobile/modules/tailfuse-engine/android/libs/tfmobile.aar
#          (minimum API 26; needs ANDROID_NDK_HOME, and ANDROID_HOME for the SDK)
#          TF_ANDROID_ABIS limits the ABIs, e.g. TF_ANDROID_ABIS=arm64 (default: all four)
#
# Installs gomobile and gobind (go install) and runs `gomobile init` when missing.
set -euo pipefail

target="${1:-}"
case "$target" in
ios | android | all) ;;
*)
	echo "usage: $0 ios|android|all" >&2
	exit 2
	;;
esac

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
engine="$(cd "$here/.." && pwd)"
module="$(cd "$engine/../../apps/mobile" && pwd)/modules/tailfuse-engine"

# Tailscale features the engine does not use, left out of the binary (smaller
# code, less memory). Kept on purpose: netstack, dns, health, logtail/netlog
# (a tailnet may require logging), c2n, posture, tailnetlock, useroutes,
# useexitnode, peerapiclient, portmapper/nattraversal/udptransport (direct
# connections), ipnbus, serve and acme (tsnet needs both to compile).
# useproxy is left out so the nodes never send their own traffic to the
# system HTTP proxy, which on the phone is Tailfuse's own router.
omit=(
	advertiseexitnode advertiseroutes androidbin androiddns appconnectors aws bird
	capture captiveportal clientupdate cloud colorable completion completion_scripts
	conn25 connreject dbus debug debugeventbus debugportmapper desktop_sessions
	dnsresolvecache doctor drive favorites flashappliance gro hujsonconf
	identityfederation iptables kube linkspeed linuxdnsfight listenrawdisco
	networkmanager oauthkey osrouter outboundproxy peerapiserver portlist qrcodes
	relayserver remoteconfig resolved runtimemetrics sdnotify serviceclientprefs ssh
	synology syslog systray taildrop tap tpm tundevstats unixsocketidentity useproxy
	wakeonlan webbrowser webclient
)
tags="$(printf 'ts_omit_%s,' "${omit[@]}")"
tags="${tags%,}"

gobin="$(go env GOBIN)"
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
export PATH="$gobin:$PATH"

# Use the gomobile version pinned in go.mod, so the tool and the bind package match.
mobile_version="$(cd "$engine" && go list -m -f '{{.Version}}' golang.org/x/mobile)"
if ! command -v gomobile >/dev/null || ! command -v gobind >/dev/null; then
	echo "installing gomobile and gobind $mobile_version"
	go install "golang.org/x/mobile/cmd/gomobile@$mobile_version" "golang.org/x/mobile/cmd/gobind@$mobile_version"
fi
if ! gomobile version >/dev/null 2>&1; then
	gomobile init
fi

common=(-tags "$tags" -trimpath -ldflags "-s -w")

build_ios() {
	if [ "$(uname -s)" != Darwin ]; then
		echo "the iOS framework can only be built on macOS (Xcode)" >&2
		exit 1
	fi
	local out="$module/ios/Frameworks/Tfmobile.xcframework"
	mkdir -p "$(dirname "$out")"
	rm -rf "$out"
	echo "building $out"
	(cd "$engine" && gomobile bind -target ios,iossimulator -iosversion 16.0 "${common[@]}" -o "$out" ./tfmobile)
	du -sh "$out"/* 2>/dev/null || true
}

build_android() {
	if [ -z "${ANDROID_NDK_HOME:-}" ]; then
		echo "ANDROID_NDK_HOME is not set: install the Android NDK (Android Studio > SDK Manager > NDK) and set ANDROID_NDK_HOME to it, e.g. \$ANDROID_HOME/ndk/<version>" >&2
		exit 1
	fi
	if [ ! -d "$ANDROID_NDK_HOME" ]; then
		echo "ANDROID_NDK_HOME=$ANDROID_NDK_HOME is not a folder" >&2
		exit 1
	fi
	local out="$module/android/libs/tfmobile.aar"
	local abis=""
	if [ -n "${TF_ANDROID_ABIS:-}" ]; then
		abis="/${TF_ANDROID_ABIS//,/,android/}"
	fi
	mkdir -p "$(dirname "$out")"
	rm -f "$out" "${out%.aar}-sources.jar"
	echo "building $out"
	(cd "$engine" && gomobile bind -target "android$abis" -androidapi 26 "${common[@]}" -o "$out" ./tfmobile)
	ls -lh "$out"
}

case "$target" in
ios) build_ios ;;
android) build_android ;;
all)
	build_ios
	build_android
	;;
esac
