// Pure helpers ported from the desktop window (app/src/main.js). No React Native imports, so
// they run under node's test runner.
import type { Device, Profile, State } from "./types";

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
  // Switched off on purpose (paused) is not waiting for a sign-in.
  const waiting = s.profiles.filter((p) => !p.online && !p.error && !p.paused);
  const off = s.profiles.filter((p) => p.paused && !p.online).length;
  const offNote = off ? ` ${off} switched off.` : "";
  const broken = s.profiles.filter((p) => p.error);
  if (broken.length) return `${broken[0].name} has a problem. See the note beside it.`;
  if (waiting.length) return `Sign in to ${waiting.map((p) => p.name).join(" and ")} to bring ${waiting.length === 1 ? "it" : "them"} online.`;
  if (online === 0) return "Every tailnet is switched off. Turn one on below.";
  return `${plural(online, "tailnet")} online.${offNote} Open any of their addresses as usual.`;
}

export type Tone = "on" | "bad" | "wait" | "";

export function tailnetState(p: Profile, up: boolean): [label: string, tone: Tone] {
  if (p.error) return ["Problem", "bad"];
  if (p.online) return ["Connected", "on"];
  if (!up) return ["Off", ""];
  if (p.paused) return ["Disconnected", ""];
  return ["Sign-in needed", "wait"];
}

export const needsSignIn = (p: Profile, up: boolean) => up && !p.online && !p.paused && !p.error;

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
export const showDeviceSearch = (count: number) => count > DEVICE_SEARCH_OVER;

// Online first, then alphabetical by name.
export function sortDevices(devices: Device[]): Device[] {
  return [...devices].sort((a, b) => Number(b.online) - Number(a.online) || a.name.localeCompare(b.name, undefined, { sensitivity: "base" }));
}

export function filterDevices(devices: Device[], query: string): Device[] {
  const q = query.trim().toLowerCase();
  if (!q) return devices;
  return devices.filter((d) => [d.name, d.dns_name, d.os, ...d.ips].some((f) => f.toLowerCase().includes(q)));
}

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
