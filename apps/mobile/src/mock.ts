// An in-memory stand-in for the engine, used where the native module is absent (web, Expo Go).
// It speaks the same JSON as engines/mobile/API.md, like the desktop's preview-stub.js.
import type { Device, Profile, State, Tunnel } from "./types";

const SIGN_IN_PAGE_AFTER_MS = 1200;
const SIGNED_IN_AFTER_MS = 6000;

type Pending = { name: string; started: number };

// Preview switches for screenshots: ?off=1 starts with Tailfuse off, ?unlocked=1 with the purchase made.
const query = typeof location !== "undefined" ? new URLSearchParams(location.search) : new URLSearchParams();
let up = !query.has("off");
let alwaysOn = false;
let unlocked = query.has("unlocked");
const FREE = 2;
let seq = 0;
let listeners = new Set<(kind: string) => void>();
const pending: Pending[] = [];

function profile(name: string, account: string, suffix: string, host: string, n: number): Profile {
  return {
    name,
    account,
    tailnet: suffix.replace(/\.ts\.net$/, ""),
    suffixes: [`.${suffix}`],
    hostname: host,
    ips: [`100.64.0.${n}`, `fd7a:115c:a1e0::${n}`],
    online: true,
    paused: false,
    error: "",
    auth_url: "",
  };
}

let profiles: Profile[] = [
  profile("home-lab", "ada@example.com", "home-lab.ts.net", "adas-iphone", 7),
  profile("work", "ada@work.example", "work.ts.net", "adas-iphone-work", 12),
];
if (query.has("empty")) profiles = [];
if (!up) profiles.forEach((p) => (p.online = false));
let tunnels: Tunnel[] = [{ name: "prod-db", listen: "127.0.0.1:5432", target: "db.work.ts.net:5432", profile: "work" }];

if (query.has("empty")) tunnels = [];

const emit = (kind = "state") => listeners.forEach((l) => l(kind));

function settle() {
  const now = Date.now();
  for (const p of [...pending]) {
    const prof = profiles.find((x) => x.name === p.name);
    if (!prof) {
      pending.splice(pending.indexOf(p), 1);
      continue;
    }
    const age = now - p.started;
    if (age >= SIGNED_IN_AFTER_MS) {
      pending.splice(pending.indexOf(p), 1);
      const n = ++seq;
      const real = `lab-${n}`;
      prof.name = real;
      prof.account = `grace@lab${n}.example`;
      prof.tailnet = real;
      prof.suffixes = [`.${real}.ts.net`];
      prof.hostname = "adas-iphone-lab";
      prof.ips = [`100.64.1.${n}`, `fd7a:115c:a1e0:1::${n}`];
      prof.online = true;
      prof.auth_url = "";
      emit("domains");
    } else if (age >= SIGN_IN_PAGE_AFTER_MS && !prof.auth_url) {
      prof.auth_url = "https://tailfuse.com/#sign-in-preview";
    }
  }
}

const matchDomains = () => profiles.flatMap((p) => p.suffixes.map((s) => s.slice(1)));

function snapshot(): State {
  settle();
  return {
    up,
    version: "0.1.26",
    proxy: "127.0.0.1:43100",
    pac_url: "http://127.0.0.1:43100/tailfuse.pac",
    unlocked,
    free_profiles: FREE,
    match_domains: matchDomains(),
    profiles: profiles.map((p) => ({ ...p, ips: [...p.ips], suffixes: [...p.suffixes] })),
    tunnels: tunnels.map((t) => ({ ...t })),
  };
}

function owner(host: string): string | null {
  const h = host.toLowerCase();
  let best: { name: string; len: number } | null = null;
  for (const p of profiles) {
    for (const s of p.suffixes) {
      const bare = s.slice(1).toLowerCase();
      if ((h === bare || h.endsWith(s.toLowerCase())) && (!best || bare.length > best.len)) best = { name: p.name, len: bare.length };
    }
  }
  return best?.name ?? null;
}

function hostOf(input: string): string {
  const t = input.trim();
  try {
    if (t.includes("://")) return new URL(t).hostname;
  } catch {
    // fall through
  }
  return t.replace(/[/:?#].*$/, "");
}

const NAMES: [string, string, boolean][] = [
  ["nas", "linux", true], ["adas-macbook", "macOS", true], ["printer-pi", "linux", true], ["office-desktop", "windows", false],
  ["pixel-8", "android", true], ["media-server", "linux", true], ["ipad", "iOS", false], ["router", "linux", true], ["build-box", "linux", false],
];
function devicesOf(p: Profile): Device[] {
  const suffix = p.suffixes[0] ?? "";
  const count = p.name === "work" ? 5 : NAMES.length;
  const own: Device = { name: p.hostname, dns_name: `${p.hostname}${suffix}.`, ips: p.ips, os: "iOS", online: true };
  return [
    own,
    ...NAMES.slice(0, count - 1).map(([n, os, online], i) => ({ name: n, dns_name: `${n}${suffix}.`, ips: [`100.64.0.${20 + i}`], os, online })),
  ];
}

type Args = Record<string, unknown>;

function run(cmd: string, a: Args): unknown {
  const needsUp = () => {
    if (!up) throw new Error("tailfuse is off");
  };
  const byName = (n: unknown) => {
    const p = profiles.find((x) => x.name === n);
    if (!p) throw new Error(`no tailnet called ${String(n)}`);
    return p;
  };
  switch (cmd) {
    case "state":
      return snapshot();
    case "add_profile": {
      needsUp();
      if (!unlocked && profiles.length >= FREE) throw new Error("that needs the unlimited upgrade");
      const name = `signing-in-${++seq}`;
      profiles.push({ name, account: "", tailnet: "", suffixes: [], hostname: "", ips: [], online: false, paused: false, error: "", auth_url: "" });
      pending.push({ name, started: Date.now() });
      return { name };
    }
    case "login": {
      needsUp();
      const p = byName(a.name);
      p.online = false;
      pending.push({ name: p.name, started: Date.now() });
      return null;
    }
    case "set_connected": {
      const p = byName(a.name);
      p.paused = !a.on;
      p.online = Boolean(a.on) && up;
      emit("domains");
      return null;
    }
    case "remove_profile": {
      const p = byName(a.name);
      profiles = profiles.filter((x) => x !== p);
      tunnels = tunnels.filter((t) => t.profile !== p.name);
      emit("domains");
      return null;
    }
    case "test_host": {
      const host = hostOf(String(a.host ?? ""));
      if (!host) throw new Error("enter an address to check");
      return { host, owner: owner(host) };
    }
    case "add_tunnel": {
      const t = { name: String(a.name ?? ""), listen: String(a.listen ?? ""), target: String(a.target ?? ""), profile: String(a.profile ?? "") };
      if (!t.name || !t.listen || !t.target) throw new Error("a local port needs a name, a port and a service");
      if (tunnels.some((x) => x.name === t.name)) throw new Error(`a local port called ${t.name} already exists`);
      if (tunnels.some((x) => x.listen === t.listen)) throw new Error("that port is already used by another local port");
      byName(t.profile);
      tunnels.push(t);
      return null;
    }
    case "remove_tunnel":
      tunnels = tunnels.filter((t) => t.name !== a.name);
      return null;
    case "set_unlocked":
      unlocked = Boolean(a.on);
      return null;
    case "devices": {
      needsUp();
      const p = byName(a.name);
      if (!p.online) throw new Error("that tailnet is off");
      return devicesOf(p);
    }
    default:
      throw new Error(`unknown command ${cmd}`);
  }
}

export const mock = {
  async call(request: string): Promise<string> {
    await new Promise((r) => setTimeout(r, 60));
    const { cmd, args } = JSON.parse(request) as { cmd: string; args?: Args };
    try {
      const result = run(cmd, args ?? {});
      if (cmd !== "state") emit();
      return JSON.stringify({ ok: true, result });
    } catch (e) {
      return JSON.stringify({ ok: false, error: (e as Error).message });
    }
  },
  async start() {
    await new Promise((r) => setTimeout(r, 300));
    up = true;
    profiles.forEach((p) => (p.online = !p.paused && p.hostname !== ""));
    emit();
  },
  async stop() {
    up = false;
    profiles.forEach((p) => (p.online = false));
    emit();
  },
  async isUp() {
    return up;
  },
  async getAlwaysOn() {
    return alwaysOn;
  },
  async setAlwaysOn(enabled: boolean) {
    alwaysOn = enabled;
  },
  subscribe(l: (kind: string) => void) {
    listeners.add(l);
    return () => void listeners.delete(l);
  },
};
