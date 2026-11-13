# tailfuse

Connect to **multiple Tailscale tailnets at once** — no VM, no daemon conflicts, no account switching. macOS, Windows, and Linux.

One embedded Tailscale node (userspace-mode `tailscaled`) per tailnet, a loopback router that dispatches traffic by hostname suffix, and a menu-bar app to drive it all.

```
┌────────────────────────── one machine ──────────────────────────┐
│  Browser / curl / ssh / db client                               │
│        │                                                        │
│        ▼                                                        │
│  tailfuse router  127.0.0.1:43100  (SOCKS5 + HTTP CONNECT + PAC)│
│   nas.home-lab.ts.net ──► profile "personal" ──► tailscaled #1  │
│   wiki.work.ts.net    ──► profile "work"     ──► tailscaled #2  │
│   anything else       ──► DIRECT (strict no-fallback)           │
└─────────────────────────────────────────────────────────────────┘
```

## Why not two Tailscale daemons?

Two full daemons fight over routes, `/etc/resolv.conf`, and firewall rules. tailfuse sidesteps this entirely: each tailnet runs in **userspace networking mode** — no TUN, no routes, no DNS changes. Your official Tailscale client (if installed) keeps owning the primary tailnet, untouched.

## Install

```sh
brew install ailio-technologies-inc/tap/tailfuse   # macOS
winget install ailio-technologies-inc.tailfuse      # Windows
# or: cargo install --git https://github.com/ailio-technologies-inc/tailfuse tailfuse-cli
```

## Quick start

```sh
tailfuse init                                   # starter config
$EDITOR ~/.config/tailfuse/config.yaml          # one profile per tailnet
tailfuse profile login personal                 # browser auth, once per tailnet
tailfuse profile login work
tailfuse up                                     # router + system PAC
```

Then just use names: `https://nas.home-lab.ts.net` and `https://wiki.work.ts.net` both load, in adjacent tabs, each on its own tailnet. `Ctrl-C` removes the PAC.

```yaml
# ~/.config/tailfuse/config.yaml
version: 1
router:
  listen: 127.0.0.1:43100
profiles:
  personal:
    suffixes: [.home-lab.ts.net]
  work:
    suffixes: [.work.ts.net, .internal.example]
    accept_routes: true          # behind a subnet router
    exit_node: exit-us           # optional per-tailnet exit node
    auth_key: tskey-auth-...     # optional unattended login
tunnels:
  - name: prod-db
    profile: work
    listen: 127.0.0.1:5432
    target: db.work.ts.net:5432  # `psql -h 127.0.0.1 -p 5432`
```

**Suffix ownership is strict** — each suffix belongs to exactly one profile, overlaps are rejected at validation, and hostnames no profile owns are *refused*, never routed elsewhere. That rejection is the isolation guarantee between tailnets.

## Usage

| Command | What it does |
|---|---|
| `tailfuse up [--no-pac]` | Start everything; sets the system PAC (macOS `networksetup` / Windows registry), clears it on exit |
| `tailfuse down` | Clear the system PAC |
| `tailfuse status` | Per-profile state, hostname, Tailscale IPs |
| `tailfuse test <host>` | Show which profile owns a hostname |
| `tailfuse ssh user@host.work.ts.net` | SSH through the owning profile (system `ssh` + ProxyCommand) |
| `tailfuse run curl http://wiki.work.ts.net` | Run any command with proxy env (`socks5h`, remote DNS) |
| `tailfuse env` | Print the proxy env for your shell |
| `tailfuse tunnel add prod-db --listen 127.0.0.1:5432 --target db.work.ts.net:5432 --profile work` | Pin a local port to a tailnet service |
| `tailfuse serve-pac` | Just the PAC URL, for manual browser configuration |

## How it works

- **Profiles** — one `tailscaled --tun=userspace-networking` subprocess per tailnet, with its own state dir, socket, and dedicated local SOCKS5 port. Nothing touches the kernel routing table.
- **Router** — a single loopback listener accepts both SOCKS5 and HTTP CONNECT, matches the destination hostname against suffix ownership (longest match wins), and forwards to that profile's SOCKS5. Unowned names get a refusal, never a fallback.
- **PAC** — a generated proxy auto-config routes only owned suffixes through the router and everything else direct; served live at `/tailfuse.pac` and applied to the OS on `up`.
- **Isolation** — profiles share no state, sockets, or routes; suffix overlaps are rejected at config validation.

## Pricing

**Free for up to 2 tailnet profiles — forever.** More profiles need a license key (one-time purchase, updates included for a year, your version keeps working after that). Keys are signed and verified offline — no phone-home, no account, no telemetry.

```sh
tailfuse license activate tailfuse_eyJ...__...
```

## License

MIT — see [LICENSE](LICENSE). `tailscaled`/`tailscale` are separate Tailscale binaries, subject to the Tailscale terms; tailfuse is an independent tool, not affiliated with or endorsed by Tailscale Inc.
