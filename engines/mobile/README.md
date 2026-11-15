# Tailfuse mobile engine

The Go engine behind Tailfuse for iPhone and Android: one embedded Tailscale node
(`tsnet.Server`) per tailnet in one process, plus a port of the desktop router rules. The
contract with the native shells and the Expo app is [API.md](API.md).

| Path | What |
|---|---|
| `config.go`, `naming.go` | `config.json` (0600, atomic writes), the desktop validation and naming rules |
| `routes.go`, `pac.go` | which tailnet owns a name (suffixes, learned domains, devices' addresses); the PAC |
| `proxy.go` | the router on 127.0.0.1:43100: SOCKS5, HTTP CONNECT, plain HTTP proxying, `/tailfuse.pac` |
| `tunnels.go` | local ports |
| `engine.go` | nodes, sign-in, the `Call` commands, change events |
| `tsnode.go` | the only file that touches tsnet (`Node` interface; tests use fakes) |
| `interfaces.go` | Android: feeds `SetInterfaces` text to Tailscale's netmon |
| `tfmobile/` | the gomobile surface |
| `cmd/tfdev/` | a harness for a computer |
| `testdata/routing.json` | routing vectors from the desktop core's tests, to compare the two cores |

## Test

```sh
cd engines/mobile
gofmt -l . && go vet ./... && go test -race ./...
```

The tests run everything over real loopback sockets with fake nodes; nothing talks to Tailscale.

## Run tfdev

```sh
go run ./cmd/tfdev -state /tmp/tf-test -listen 127.0.0.1:43199 -v
{"cmd":"add_profile"}
{"cmd":"state"}            # the new profile's auth_url appears within a few seconds
{"cmd":"test_host","args":{"host":"nas.tail1234.ts.net"}}
stop                       # then try the "works off" commands
```

One JSON request per line on stdin, one response per line on stdout, change events as
`event: state` / `event: domains`. Use `-listen` with another port while desktop Tailfuse is
running (it holds 43100). Then point a client at the router, e.g.
`curl -x http://127.0.0.1:43199 http://nas.tail1234.ts.net/`.

## Build the bindings

```sh
scripts/build-engine.sh ios       # apps/mobile/modules/tailfuse-engine/ios/Frameworks/Tfmobile.xcframework
scripts/build-engine.sh android   # apps/mobile/modules/tailfuse-engine/android/libs/tfmobile.aar (needs ANDROID_NDK_HOME)
scripts/build-engine.sh all
```

The script installs gomobile (the version pinned in `go.mod`) when missing and builds with the
`ts_omit_*` tags listed in it, which leave out Tailscale features the engine does not use. iOS:
device arm64 and simulator arm64/x86_64, minimum iOS 16. Android: minimum API 26.
