#!/bin/sh
# Runs the real-node test (two tailnets on Tailscale's testcontrol + DERP,
# WireGuard between in-process tsnet nodes, the phone's nodes on Mux links).
#
# It needs Tailscale's test packages, which pull ~100 modules the engine does
# not otherwise use, so it runs in a temporary copy of the module: go.mod in
# the repo stays as it is. From engines/mobile:
#
#   mise exec -- sh packets/testdata/integration/run.sh
set -eu
here=$(cd "$(dirname "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp "$here"/go.mod "$here"/go.sum "$here"/*.go "$tmp"/
cp -R "$here"/packets "$tmp"/packets
cp -R "$here"/tfmobile "$tmp"/tfmobile
cp "$here"/packets/testdata/integration/tsnet_integration_test.go "$tmp"/packets/
cd "$tmp"
go mod tidy >/dev/null
go test -tags tsintegration -run Tsnet -count=1 -v ./packets/ 2>&1 | grep -E '^(---|PASS|FAIL|ok)|tsnet_integration_test'
