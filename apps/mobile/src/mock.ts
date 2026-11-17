// An in-memory stand-in for the engine, used where the native module is absent (web, Expo Go).
// It speaks the same JSON as engines/mobile/API.md, like the desktop's preview-stub.js.
import { decodeSyncLink } from "./logic";
import type { Device, DevicePage, Profile, State, SyncResult, Tunnel } from "./types";

// The shared test vector (engines/mobile/testdata/sync.json), for the preview.
export const SAMPLE_SYNC_LINK =
  "tailfuse://sync?v=1&d=eyJ2IjoxLCJmcm9tIjoiQWRhJ3MgTWFjQm9vayIsInByb2ZpbGVzIjpbeyJuYW1lIjoicGVyc29uYWwiLCJhY2NvdW50IjoiYWRhQGV4YW1wbGUuY29tIiwic3VmZml4ZXMiOlsiLmhvbWUtbGFiLnRzLm5ldCJdfSx7Im5hbWUiOiJ3b3JrIiwiYWNjb3VudCI6ImFkYUB3b3JrLmV4YW1wbGUiLCJzdWZmaXhlcyI6WyIud29yay50cy5uZXQiLCIuaW50ZXJuYWwuZXhhbXBsZSJdLCJhY2NlcHRfcm91dGVzIjp0cnVlLCJleGl0X25vZGUiOiJleGl0LXVzIn1dLCJ0dW5uZWxzIjpbeyJuYW1lIjoicHJvZC1kYiIsImxpc3RlbiI6IjEyNy4wLjAuMTo1NDMyIiwidGFyZ2V0IjoiZGIud29yay50cy5uZXQ6NTQzMiIsInByb2ZpbGUiOiJ3b3JrIn1dfQ";

const SIGN_IN_PAGE_AFTER_MS = 1200;
const SIGNED_IN_AFTER_MS = 6000;

type Pending = { name: string; started: number; existing?: boolean };

// Preview switches for screenshots: ?off=1 starts with Tailfuse off, ?unlocked=1 with the purchase made.
const query = typeof location !== "undefined" ? new URLSearchParams(location.search) : new URLSearchParams();
let up = !query.has("off");
let alwaysOn = false;
let unlocked = query.has("unlocked");
// ?proxy=1 starts in proxy mode, ?allappserror=1 with all-apps mode fallen back for the session.
let allApps = !query.has("proxy");
let allAppsError = query.has("allappserror") ? "no utun file descriptor in this process" : "";
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
    needs_login: false,
    connecting: false,
  };
}

let profiles: Profile[] = [
  profile("home-lab", "ada@example.com", "home-lab.ts.net", "adas-iphone", 7),
  profile("work", "ada@work.example", "work.ts.net", "adas-iphone-work", 12),
];
if (query.has("empty") || query.get("sheet")?.startsWith("sync")) profiles = [];
if (!up) profiles.forEach((p) => (p.online = false));
// ?connecting=1: Tailfuse is on and the tailnets are still coming up.
if (query.has("connecting")) profiles.forEach((p) => ((p.online = false), (p.connecting = true)));
let tunnels: Tunnel[] = [{ name: "prod-db", listen: "127.0.0.1:5432", target: "db.work.ts.net:5432", profile: "work" }];

if (query.has("empty") || query.get("sheet")?.startsWith("sync")) tunnels = [];
if (query.has("domains")) profiles.find((p) => p.name === "work")?.suffixes.push(".ailio.dev");

const FREE_LIMIT_ERR = "that needs the unlimited upgrade";

function importSetup(link: string, dryRun: boolean): SyncResult {
  if (!/^tailfuse:\/\/sync\?/i.test(link.trim())) throw new Error("that is not a tailfuse sync code");
  const v = /[?&]v=(\d+)/.exec(link);
  if (v && Number(v[1]) > 1) throw new Error("that code is from a newer tailfuse");
  const setup = decodeSyncLink(link);
  if (!setup) throw new Error("that is not a tailfuse sync code");
  const res: SyncResult = { from: setup.from, added: [], already: [], needs_unlock: [], tunnels_added: [], skipped: [] };
  const local = new Map<string, string>();
  const locked = new Set<string>();
  const names = new Set(profiles.map((p) => p.name));
  const owned = new Map(profiles.flatMap((p) => p.suffixes.map((s) => [s.toLowerCase(), p.name] as const)));
  const add: Profile[] = [];
  for (const sp of setup.profiles) {
    const suffixes = sp.suffixes.map((s) => s.trim().toLowerCase()).filter((s) => s.startsWith(".") && s.length > 1);
    if (suffixes.length === 0 || suffixes.length !== sp.suffixes.length) {
      res.skipped.push({ name: sp.name, reason: "its addresses are not valid" });
      continue;
    }
    const existing = suffixes.map((s) => owned.get(s)).find(Boolean);
    if (existing) {
      res.already.push(sp.name);
      local.set(sp.name, existing);
    } else if (!unlocked && names.size >= FREE) {
      res.needs_unlock.push(sp.name);
      locked.add(sp.name);
    } else {
      let name = /^[a-z0-9-]{1,40}$/.test(sp.name) && !names.has(sp.name) ? sp.name : sp.name.toLowerCase().replace(/[^a-z0-9-]/g, "") || "tailnet";
      for (let n = 2; names.has(name); n++) name = `${name.replace(/-\d+$/, "")}-${n}`;
      names.add(name);
      suffixes.forEach((s) => owned.set(s, name));
      local.set(sp.name, name);
      res.added.push(sp.name);
      add.push({ name, account: "", tailnet: "", suffixes, hostname: "", ips: [], online: false, paused: false, error: "", auth_url: "", needs_login: true, connecting: false });
    }
  }
  const taken = new Set(tunnels.map((t) => t.name));
  const listens = new Set(tunnels.map((t) => t.listen));
  const addT: Tunnel[] = [];
  for (const t of setup.tunnels) {
    const prof = local.get(t.profile);
    if (locked.has(t.profile)) res.skipped.push({ name: t.name, reason: "its tailnet needs the unlock" });
    else if (!prof) res.skipped.push({ name: t.name, reason: "its tailnet is not on this phone" });
    else if (taken.has(t.name)) res.skipped.push({ name: t.name, reason: "a local port with that name already exists" });
    else if (listens.has(t.listen)) res.skipped.push({ name: t.name, reason: "that port is already used by another local port" });
    else {
      taken.add(t.name);
      listens.add(t.listen);
      addT.push({ name: t.name, listen: t.listen, target: t.target, profile: prof });
      res.tunnels_added.push(t.name);
    }
  }
  if (!dryRun) {
    profiles.push(...add);
    tunnels.push(...addT);
    if (add.length) emit("domains");
  }
  return res;
}


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
    if (age >= SIGNED_IN_AFTER_MS && p.existing) {
      pending.splice(pending.indexOf(p), 1);
      prof.online = up;
      prof.needs_login = false;
      prof.auth_url = "";
      prof.account = prof.account || `ada@${prof.name}.example`;
      prof.tailnet = prof.tailnet || prof.name;
      prof.hostname = "adas-iphone";
      prof.ips = [`100.64.2.${++seq}`];
      emit("domains");
    } else if (age >= SIGNED_IN_AFTER_MS) {
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
      prof.needs_login = false;
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
    all_apps: allApps,
    all_apps_active: up && allApps && !allAppsError,
    all_apps_error: allAppsError,
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
// ?devices=5000 makes every tailnet that big, with names like a company's.
const BIG = Math.max(0, Number(query.get("devices")) || 0);
const TEAMS = ["eng", "sales", "design", "ops", "finance", "support", "legal", "hr", "data", "qa"];
const KINDS: [string, string][] = [["laptop", "macOS"], ["laptop", "windows"], ["desktop", "windows"], ["iphone", "iOS"], ["ipad", "iOS"], ["pixel", "android"], ["srv", "linux"], ["ci-runner", "linux"], ["kiosk", "linux"]];
const SITES = ["us-east", "us-west", "eu-central", "ap-south", "lon", "nyc"];
const bigCache = new Map<string, Device[]>();

function bigDevices(p: Profile, count: number): Device[] {
  const hit = bigCache.get(p.name);
  if (hit && hit.length === count) return hit;
  const suffix = p.suffixes[0] ?? "";
  let r = 12345;
  const rnd = (n: number) => ((r = (r * 1103515245 + 12345) & 0x7fffffff), r % n);
  const out: Device[] = [{ name: p.hostname, dns_name: `${p.hostname}${suffix}.`, ips: p.ips, os: "iOS", online: true, self: true }];
  for (let i = 1; i < count; i++) {
    const [kind, os] = KINDS[rnd(KINDS.length)];
    const team = TEAMS[rnd(TEAMS.length)];
    let name = kind === "srv" || kind === "ci-runner" ? `${kind}-${SITES[rnd(SITES.length)]}-${String(i).padStart(4, "0")}` : `${team}-${kind}-${String(i).padStart(4, "0")}`;
    if (i % 997 === 0) name = `${team}-conference-room-${kind}-with-a-very-long-descriptive-name-${i}`;
    out.push({ name, dns_name: `${name}${suffix}.`, ips: [`100.${64 + (i >> 16)}.${(i >> 8) & 255}.${i & 255}`, `fd7a:115c:a1e0::${i.toString(16)}`], os, online: rnd(100) < 38 });
  }
  bigCache.set(p.name, out);
  return out;
}

function devicesOf(p: Profile): Device[] {
  if (BIG) return bigDevices(p, BIG);
  const suffix = p.suffixes[0] ?? "";
  const count = p.name === "work" ? 5 : NAMES.length;
  const own: Device = { name: p.hostname, dns_name: `${p.hostname}${suffix}.`, ips: p.ips, os: "iOS", online: true, self: true };
  return [
    own,
    ...NAMES.slice(0, count - 1).map(([n, os, online], i) => ({ name: n, dns_name: `${n}${suffix}.`, ips: [`100.64.0.${20 + i}`], os, online })),
  ];
}

// The engine's `devices` reply: this device first, online next, then by name; filtered and paged.
function devicePage(p: Profile, a: Record<string, unknown>): DevicePage {
  const all = [...devicesOf(p)].sort((x, y) => Number(!!y.self) - Number(!!x.self) || Number(y.online) - Number(x.online) || x.name.toLowerCase().localeCompare(y.name.toLowerCase()));
  const q = String(a.query ?? "").trim().toLowerCase();
  const matched = all.filter((d) => (!a.online || d.online) && (!q || [d.name, d.dns_name, d.os, ...d.ips].some((f) => f.toLowerCase().includes(q))));
  const limit = Math.min(Math.max(Number(a.limit) || 50, 1), 200);
  const offset = Math.max(Number(a.offset) || 0, 0);
  return { devices: matched.slice(offset, offset + limit), total: all.length, online: all.filter((d) => d.online).length, matched: matched.length };
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
      if (!unlocked && profiles.length >= FREE) throw new Error(FREE_LIMIT_ERR);
      const name = `signing-in-${++seq}`;
      profiles.push({ name, account: "", tailnet: "", suffixes: [], hostname: "", ips: [], online: false, paused: false, error: "", auth_url: "", needs_login: true, connecting: false });
      pending.push({ name, started: Date.now() });
      return { name };
    }
    case "login": {
      needsUp();
      const p = byName(a.name);
      p.online = false;
      p.paused = false;
      p.needs_login = true;
      pending.push({ name: p.name, started: Date.now(), existing: true });
      return null;
    }
    case "import_setup":
      return importSetup(String(a.link ?? ""), Boolean(a.dry_run));
    case "add_domain": {
      const p = byName(a.name);
      const d = String(a.domain ?? "").trim().toLowerCase().replace(/^\*?\./, "").replace(/\.$/, "");
      if (!d) throw new Error("enter a domain, such as example.com");
      if (!d.includes(".")) throw new Error(`"${d}" is a whole top-level domain; enter a domain of your own, such as example.com`);
      if (/^[\d.]+$/.test(d) || d.includes(":")) throw new Error("that is an address; enter a domain name, such as example.com");
      if (d.endsWith("ts.net")) throw new Error("a ts.net name belongs to the tailnet's own sign-in and is added for you");
      const other = profiles.find((x) => x !== p && x.suffixes.includes(`.${d}`));
      if (other) throw new Error(`${d} already belongs to the tailnet "${other.name}"; remove it there first`);
      if (!p.suffixes.includes(`.${d}`)) p.suffixes.push(`.${d}`);
      emit("domains");
      return null;
    }
    case "remove_domain": {
      const p = byName(a.name);
      const d = String(a.domain ?? "").trim().toLowerCase().replace(/^\*?\./, "");
      if (d.endsWith("ts.net")) throw new Error("the tailnet's own name cannot be removed");
      if (!p.suffixes.includes(`.${d}`)) throw new Error(`"${p.name}" does not own ${d}`);
      p.suffixes = p.suffixes.filter((s) => s !== `.${d}`);
      emit("domains");
      return null;
    }
    case "set_connected": {
      const p = byName(a.name);
      p.paused = !a.on;
      p.online = Boolean(a.on) && up;
      emit("domains");
      return null;
    }
    case "set_options": {
      const p = byName(a.name);
      p.prefs = {
        exit_node: String(a.exit_node ?? ""),
        accept_routes: Boolean(a.accept_routes),
        shields_up: Boolean(a.shields_up),
      };
      emit("state");
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
      const own = owner(host);
      // Preview: ailio.dev stands for a custom domain that points at a device on the work tailnet.
      if (!own && up && /(^|\.)ailio\.dev$/.test(host)) return { host, owner: "work", via: "address", suggest_domain: "ailio.dev" };
      return { host, owner: own };
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
    case "set_all_apps":
      if (typeof a.on !== "boolean") throw new Error("bad arguments: on is required");
      allApps = a.on;
      allAppsError = "";
      return null;
    case "devices": {
      needsUp();
      const p = byName(a.name);
      if (!p.online) throw new Error("that tailnet is off");
      return devicePage(p, a);
    }
    default:
      throw new Error(`unknown command ${cmd}`);
  }
}

export const mock = {
  async call(request: string): Promise<string> {
    await new Promise((r) => setTimeout(r, 60));
    const { cmd, args } = JSON.parse(request) as { cmd: string; args?: Args };
    // ?slow=1: `devices` never answers in time to look at (loading state in screenshots).
    if (cmd === "devices" && query.has("slow")) await new Promise((r) => setTimeout(r, 120000));
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
    profiles.forEach((p) => (p.connecting = false));
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
