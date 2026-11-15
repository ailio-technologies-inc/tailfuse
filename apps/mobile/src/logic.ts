// Pure helpers ported from the desktop window (app/src/main.js). No React Native imports, so
// they run under node's test runner.
import type { Device, DevicePage, Profile, State, SyncResult } from "./types";

// Engine messages arrive lower-case and unpunctuated ("tailfuse is off").
export function sentence(text: unknown): string {
  const raw = text instanceof Error ? text.message : String(text ?? "");
  const t = raw.trim().replace(/^Error:\s*/, "");
  if (!t) return "";
  const cased = t[0].toUpperCase() + t.slice(1);
  return /[.!?]$/.test(cased) ? cased : `${cased}.`;
}

// Turn the engine's raw messages into [title, text] a person can act on.
export function friendly(err: unknown): [string, string] {
  const raw = err instanceof Error ? err.message : String(err ?? "");
  if (/tailfuse is off/i.test(raw)) {
    return ["Tailfuse is off", "Turn Tailfuse on with the switch at the top, then try again."];
  }
  if (/needs the unlimited upgrade/i.test(raw)) {
    return ["That needs the unlock", "Two tailnets are free. Open Unlimited tailnets below to add as many as you need."];
  }
  if (/address already in use|router:/i.test(raw)) {
    return ["Tailfuse's port is busy", "Another program, or a second copy of Tailfuse, is using port 43100. Quit it and turn Tailfuse on again."];
  }
  if (/not finished in time|ran out of time|timed out/i.test(raw)) {
    return ["The sign-in ran out of time", "Start it again and finish in the browser page within a few minutes."];
  }
  if (/not a tailfuse sync code/i.test(raw)) {
    return ["That isn't a Tailfuse code", "Scan the code Tailfuse shows on your computer, under Settings, Set up your phone."];
  }
  if (/newer tailfuse/i.test(raw)) {
    return ["That code is from a newer Tailfuse", "Update Tailfuse on this phone, then scan it again."];
  }
  if (/already belongs to the tailnet/i.test(raw)) return ["That domain is taken", sentence(raw)];
  if (/no profiles defined/i.test(raw)) {
    return ["Add a tailnet first", "Sign in with Tailscale under Your tailnets, then turn Tailfuse on."];
  }
  return ["Something went wrong", sentence(raw)];
}

export const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

// The one line under the header: where things stand and what to do next.
export function statusLine(s: Pick<State, "up" | "profiles">): string {
  const total = s.profiles.length;
  if (total === 0) return "Add a tailnet below to get started.";
  if (!s.up) return `Tailfuse is off. Turn it on to reach your ${total === 1 ? "tailnet" : `${total} tailnets`}.`;
  const online = s.profiles.filter((p) => p.online).length;
  const off = s.profiles.filter((p) => p.paused && !p.online).length;
  const offNote = off ? ` ${off} switched off.` : "";
  const broken = s.profiles.filter((p) => p.error);
  if (broken.length) return `${broken[0].name} has a problem. See the note beside it.`;
  // Only a sign-in the engine knows is needed is asked for; one still coming up is just connecting.
  const connecting = s.profiles.filter((p) => isConnecting(p, true));
  if (connecting.length) return connecting.length === 1 ? `Connecting to ${connecting[0].name}…` : `Connecting to ${plural(connecting.length, "tailnet")}…`;
  const waiting = s.profiles.filter((p) => showSignIn(p));
  if (waiting.length) return `Sign in to ${waiting.map((p) => p.name).join(" and ")} to bring ${waiting.length === 1 ? "it" : "them"} online.`;
  if (online === 0) return "Every tailnet is switched off. Turn one on below.";
  return `${plural(online, "tailnet")} online.${offNote} Open any of their addresses as usual.`;
}

export type Tone = "on" | "bad" | "wait" | "";

// Tailfuse is on, not paused, and the engine knows of nothing wrong or missing: the node is on its way up.
export const isConnecting = (p: Profile, up: boolean) => up && !p.online && !p.paused && !p.error && !p.needs_login;

// Row state, in this order: problem, connected, sign-in needed (only when the engine says so), off,
// paused, connecting. A tailnet that is merely off or coming up never asks for a sign-in.
export function tailnetState(p: Profile, up: boolean): [label: string, tone: Tone] {
  if (p.error) return ["Problem", "bad"];
  if (p.online) return ["Connected", "on"];
  if (p.needs_login) return ["Sign-in needed", "wait"];
  if (!up) return ["Off", ""];
  if (p.paused) return ["Disconnected", ""];
  return ["Connecting…", "wait"];
}

export const showSignIn = (p: Profile) => !p.online && !p.error && p.needs_login;

// The switch reads on while the tailnet is connected or coming up.
export const switchOn = (p: Profile, up: boolean) => p.online || isConnecting(p, up);

export function details(p: Profile, device: string): [string, string][] {
  const rows: [string, string][] = [];
  if (p.hostname) rows.push([`This ${device}'s name`, p.hostname]);
  for (const ip of p.ips.filter(Boolean)) rows.push([ip.includes(":") ? "IPv6" : "IPv4", ip]);
  if (p.account) rows.push(["Name in Tailfuse", p.name]);
  return rows;
}

export function checkResult(host: string, owner: string | null): string {
  return owner
    ? `${host} goes to ${owner}.`
    : `${host} is on none of your tailnets. Tailfuse leaves it alone, and your apps reach it directly.`;
}

// ----- the free limit and the one-time unlock -----

export const FREE_TAILNETS = 2;

export const freeLimit = (s: Pick<State, "free_profiles">) => s.free_profiles ?? FREE_TAILNETS;

export const atLimit = (s: Pick<State, "profiles" | "unlocked" | "free_profiles">) => !s.unlocked && s.profiles.length >= freeLimit(s);

// The note beside "Your tailnets".
export function tailnetsHint(s: Pick<State, "profiles" | "unlocked" | "free_profiles">): string {
  const n = s.profiles.length;
  if (n === 0) return "";
  if (s.unlocked) return `${plural(n, "tailnet")}, unlocked`;
  const free = freeLimit(s);
  return n <= free ? `${n} of ${free} free ${free === 1 ? "tailnet" : "tailnets"} used` : plural(n, "tailnet");
}

// The add-card's line under its title.
export function addSubtext(s: Pick<State, "profiles" | "unlocked" | "free_profiles">): string {
  const base = "Sign in with Tailscale in your browser.";
  if (atLimit(s)) return `${base} ${freeLimit(s) === 2 ? "A third tailnet" : "Another tailnet"} needs the one-time unlock.`;
  return `${base} The tailnet, its name and its account fill themselves in.`;
}

export const unlockLabel = (price?: string | null) => (price ? `Unlock for ${price}` : "Unlock");

// Store failures in plain words. A cancelled purchase is handled by the caller and shows nothing.
export function storeErrorText(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err ?? "");
  if (/network|offline|internet|connect|timed? ?out|unavailable/i.test(raw)) {
    return "Couldn't reach the store. Check your connection and try again.";
  }
  if (/not available|not in this build|not prepared/i.test(raw)) {
    return "Purchases aren't available in this build of Tailfuse.";
  }
  return "The store couldn't finish that. Try again in a moment.";
}

// ----- devices on a tailnet -----

export const DEVICE_SEARCH_OVER = 8;
export const showDeviceSearch = (total: number) => total > DEVICE_SEARCH_OVER;
export const DEVICE_PAGE = 50;
export const DEVICE_SEARCH_DEBOUNCE_MS = 250;
export const DEVICE_ROW_HEIGHT = 64;

/** A whole number in the device's own locale ("1,204" or "1.204"). */
export function formatCount(n: number, locale?: string): string {
  try {
    return new Intl.NumberFormat(locale).format(n);
  } catch {
    return String(n);
  }
}

/** "1,204 devices · 37 online". */
export function deviceSummary(total: number, online: number, locale?: string): string {
  return `${formatCount(total, locale)} ${total === 1 ? "device" : "devices"} · ${formatCount(online, locale)} online`;
}

/** The first `max` items and a "+N more" note for the rest ("" when nothing is left out). */
export function capList<T>(items: readonly T[], max: number): { shown: T[]; more: string; hidden: number } {
  const hidden = Math.max(0, items.length - max);
  return { shown: items.slice(0, max), more: hidden ? `+${hidden} more` : "", hidden };
}

export const DOMAIN_CAP = 5;
export const showAllLabel = (n: number) => `Show all (${n})`;

/** Message when the filters leave nothing. */
export const noDevicesText = (query: string, onlineOnly: boolean) =>
  query.trim() ? `No devices match “${query.trim()}”` : onlineOnly ? "No devices are online" : "No devices yet";

export type DevicesState = {
  /** The tailnet the list belongs to. */
  key: string;
  /** Id of the latest request; replies from any other are stale and ignored. */
  req: number;
  status: "idle" | "loading" | "ready" | "error";
  loadingMore: boolean;
  devices: Device[];
  total: number;
  online: number;
  matched: number;
  error: string;
};

export const devicesInitial: DevicesState = { key: "", req: 0, status: "idle", loadingMore: false, devices: [], total: 0, online: 0, matched: 0, error: "" };

export type DevicesAction =
  | { type: "fetch"; key: string; req: number; append: boolean }
  | { type: "loaded"; req: number; offset: number; page: DevicePage }
  | { type: "failed"; req: number; text: string }
  | { type: "reset" };

export function devicesReducer(s: DevicesState, a: DevicesAction): DevicesState {
  switch (a.type) {
    case "reset":
      return { ...devicesInitial, req: s.req };
    case "fetch": {
      // A new tailnet starts empty (skeleton rows); a new search keeps the old rows until the reply lands.
      const keep = a.key === s.key && s.status === "ready";
      return {
        ...(keep ? s : { ...devicesInitial, key: a.key }),
        key: a.key,
        req: a.req,
        status: keep ? "ready" : "loading",
        loadingMore: a.append,
        error: "",
      };
    }
    case "loaded": {
      if (a.req !== s.req) return s; // out of order: a newer request was made since
      if (a.offset > 0 && a.offset !== s.devices.length) return s;
      const devices = a.offset === 0 ? a.page.devices : [...s.devices, ...a.page.devices];
      return { ...s, status: "ready", loadingMore: false, devices, total: a.page.total, online: a.page.online, matched: a.page.matched, error: "" };
    }
    case "failed":
      if (a.req !== s.req) return s;
      return { ...s, status: s.devices.length && s.loadingMore ? "ready" : "error", loadingMore: false, error: a.text };
  }
}

export const hasMoreDevices = (s: DevicesState) => s.status === "ready" && !s.loadingMore && !s.error && s.devices.length < s.matched;

// Tailscale reports DNS names with a trailing dot.
export const deviceAddress = (dns: string) => dns.replace(/\.$/, "");
export const deviceUrl = (dns: string) => `https://${deviceAddress(dns)}`;

const OS_NAMES: Record<string, string> = { ios: "iOS", macos: "macOS", linux: "Linux", windows: "Windows", android: "Android", freebsd: "FreeBSD", tvos: "tvOS" };
export function osLabel(os: string): string {
  const k = os.trim().toLowerCase();
  return OS_NAMES[k] ?? (k ? k[0].toUpperCase() + k.slice(1) : "");
}

// The state label under a tailnet row is only worth repeating when it says something the dot and
// the switch don't already say.
export const showStateLabel = (label: string) => label !== "Connected" && label !== "Off";

// ----- sync from a computer (engines/mobile/API.md, "Sync from a computer") -----

export type SyncProfile = { name: string; account: string; suffixes: string[] };
export type SyncSetup = { v: number; from: string; profiles: SyncProfile[]; tunnels: { name: string; listen: string; target: string; profile: string }[] };

const SYNC_RE = /^tailfuse:\/\/sync\?/i;

/** Is this text a Tailfuse sync link? (A shape check; the engine does the real validation.) */
export function isSyncLink(text: string): boolean {
  const t = text.trim();
  return SYNC_RE.test(t) && /[?&]d=[A-Za-z0-9_-]+/.test(t);
}

/** Pulls a sync link out of pasted text or a scanned code; null when there is none. */
export function findSyncLink(text: string): string | null {
  const m = /tailfuse:\/\/sync\?\S+/i.exec(text);
  return m && isSyncLink(m[0]) ? m[0] : null;
}

/** Reads a link's setup for the confirmation screen; null when it cannot be read. */
export function decodeSyncLink(link: string): SyncSetup | null {
  const m = /[?&]d=([A-Za-z0-9_-]+)/.exec(link.trim());
  if (!m) return null;
  try {
    const b64 = m[1].replace(/-/g, "+").replace(/_/g, "/");
    const bin = atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4));
    const json = decodeURIComponent(Array.from(bin, (c) => "%" + c.charCodeAt(0).toString(16).padStart(2, "0")).join(""));
    const s = JSON.parse(json) as Partial<SyncSetup>;
    if (typeof s !== "object" || s === null || s.v !== 1) return null;
    return {
      v: 1,
      from: typeof s.from === "string" ? s.from : "",
      profiles: (Array.isArray(s.profiles) ? s.profiles : []).map((p) => ({
        name: String(p?.name ?? ""),
        account: String(p?.account ?? ""),
        suffixes: Array.isArray(p?.suffixes) ? p.suffixes.map(String) : [],
      })),
      tunnels: Array.isArray(s.tunnels) ? s.tunnels : [],
    };
  } catch {
    return null;
  }
}

export type SyncStatus = "add" | "already" | "unlock" | "skipped";
export type SyncRow = { name: string; title: string; detail: string; status: SyncStatus; label: string };

const STATUS_LABEL: Record<SyncStatus, string> = { add: "Will be added", already: "Already here", unlock: "Needs the unlock", skipped: "Left out" };

/** The confirmation screen: one row per tailnet in the code, with what the dry run said about it. */
export function syncRows(setup: SyncSetup, r: SyncResult): SyncRow[] {
  return setup.profiles.map((p) => {
    const status: SyncStatus = r.added.includes(p.name) ? "add" : r.already.includes(p.name) ? "already" : r.needs_unlock.includes(p.name) ? "unlock" : "skipped";
    return {
      name: p.name,
      title: p.account || p.name,
      detail: p.suffixes.map((s) => `*${s}`).join("  "),
      status,
      label: STATUS_LABEL[status],
    };
  });
}

export const syncPortCount = (r: SyncResult) => r.tunnels_added.length;
export const syncPortsLine = (r: SyncResult) => (r.tunnels_added.length ? plural(r.tunnels_added.length, "local port") : "");
export const syncFromLine = (setup: SyncSetup) => (setup.from ? `From ${setup.from}` : "From your computer");
export const syncAddCount = (r: SyncResult) => r.added.length;

/** The primary button: "Add 2 tailnets", or the disabled "Nothing new to add". */
export function syncButton(r: SyncResult): { title: string; disabled: boolean } {
  const n = syncAddCount(r);
  const ports = r.tunnels_added.length;
  if (n === 0 && ports === 0) return { title: "Nothing new to add", disabled: true };
  if (n === 0) return { title: `Add ${plural(ports, "local port")}`, disabled: false };
  return { title: `Add ${plural(n, "tailnet")}`, disabled: false };
}

export const syncNote = (device: string) => `No passwords or keys are in the code. You sign in to each tailnet once on this ${device}.`;
export const syncUnlockNote = (r: SyncResult) => (r.needs_unlock.length ? "Two tailnets are free. Unlimited tailnets adds the rest." : "");

export function syncDoneText(r: SyncResult): string {
  const n = r.added.length;
  if (n === 0) return `${plural(r.tunnels_added.length, "local port")} added.`;
  return `${plural(n, "tailnet")} added. Sign in to ${n === 1 ? "it" : "each one"} to bring ${n === 1 ? "it" : "them"} online.`;
}

// ----- domains a tailnet owns -----

export type DomainRow = { suffix: string; domain: string; builtIn: boolean };

/** A tailnet's suffixes for the Domains group; its own ts.net name is built in and cannot be removed. */
export const domainRows = (p: Pick<Profile, "suffixes">): DomainRow[] =>
  p.suffixes.map((s) => ({ suffix: s, domain: s.replace(/^\./, ""), builtIn: s.toLowerCase().endsWith(".ts.net") }));

export const DOMAIN_HINT = "For a domain of your own that points at a device on this tailnet, such as a site at example.com.";
export const suggestLine = (domain: string, tailnet: string) => `${domain} points at a device on ${tailnet}.`;
export const suggestButton = (domain: string, tailnet: string) => `Always send ${domain} to ${tailnet}`;

// ----- polling without churn -----

/**
 * The next state to keep: `prev` itself when nothing changed, otherwise `next` with every unchanged
 * profile and tunnel keeping its old object, so memoised rows skip rendering on each 3-second poll.
 */
export function stabilize(prev: State | null, next: State): State {
  if (!prev) return next;
  const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
  if (same(prev, next)) return prev;
  const reuse = <T extends { name: string }>(old: T[], now: T[]) => now.map((x) => old.find((o) => o.name === x.name && same(o, x)) ?? x);
  const profiles = reuse(prev.profiles, next.profiles);
  const tunnels = reuse(prev.tunnels, next.tunnels);
  return {
    ...next,
    profiles: profiles.length === prev.profiles.length && profiles.every((p, i) => p === prev.profiles[i]) ? prev.profiles : profiles,
    tunnels: tunnels.length === prev.tunnels.length && tunnels.every((p, i) => p === prev.tunnels[i]) ? prev.tunnels : tunnels,
    match_domains: same(prev.match_domains, next.match_domains) ? prev.match_domains : next.match_domains,
  };
}

// ----- the upgrade modal -----

/** The engine's refusal at the free limit. */
export const isLimitError = (err: unknown) => /needs the unlimited upgrade/i.test(err instanceof Error ? err.message : String(err ?? ""));

export type UpgradeSource = "add" | "sync" | "error";

/** What the person was doing when the limit stopped them; the modal continues it after a purchase. */
export type UpgradeResume = "add" | "sync" | null;

/** Whether tapping "add a tailnet" opens the upgrade modal rather than starting a sign-in. */
export const addNeedsUpgrade = (s: Pick<State, "profiles" | "unlocked" | "free_profiles">) => atLimit(s);

export const upgradeIntro = (account: string) => `Two tailnets are free. A one-time purchase adds as many as you need, on this ${account}.`;

// Non-consumable purchases belong to the store account on both stores and come back with Restore purchase.
export const upgradeBullets = (account: string): string[] => [
  "As many tailnets as you need",
  "One purchase, no subscription",
  `Works on every device signed in to the same ${account} (use Restore purchase)`,
];

export const SYNC_UNLOCK_BUTTON = "Unlock to add the rest";
