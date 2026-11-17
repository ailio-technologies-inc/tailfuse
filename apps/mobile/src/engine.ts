// Typed wrapper over the engine. Uses the native module when it is linked, the in-memory mock
// otherwise. Every call parses {ok,result,error} and throws Error(error) on failure.
import * as Native from "../modules/tailfuse-engine";
import { mock } from "./mock";
import type { DevicePage, FileTarget, Prefs, State, WaitingFile, SyncResult, TestHostResult } from "./types";

export const usingMock = !Native.isAvailable;

const backend = usingMock ? mock : Native;

async function run<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const raw = await backend.call(JSON.stringify(args ? { cmd, args } : { cmd }));
  let res: { ok: boolean; result?: T; error?: string };
  try {
    res = JSON.parse(raw);
  } catch {
    throw new Error("the engine sent something unreadable");
  }
  if (!res.ok) throw new Error(res.error || "the engine failed");
  return res.result as T;
}

export async function getState(): Promise<State> {
  const s = await run<State>("state");
  // `state` works while Tailfuse is off; `up` then means nothing on iOS' app side, so ask the shell.
  const up = (await backend.isUp()) ? s.up !== false : false;
  return { ...s, up, profiles: s.profiles ?? [], tunnels: s.tunnels ?? [], match_domains: s.match_domains ?? [] };
}

export const addProfile = (authKey?: string) => run<{ name: string }>("add_profile", authKey ? { auth_key: authKey } : undefined);
export const login = (name: string) => run<null>("login", { name });
export const setConnected = (name: string, on: boolean) => run<null>("set_connected", { name, on });
export const removeProfile = (name: string) => run<null>("remove_profile", { name });
export const testHost = (host: string) => run<TestHostResult>("test_host", { host });
export const addTunnel = (t: { name: string; listen: string; target: string; profile: string }) => run<null>("add_tunnel", t);
export const removeTunnel = (name: string) => run<null>("remove_tunnel", { name });
export type DevicesQuery = { name: string; query?: string; online?: boolean; limit?: number; offset?: number; refresh?: boolean };
export const devices = (q: DevicesQuery) => run<DevicePage>("devices", q);
export const importSetup = (link: string, dryRun = false) => run<SyncResult>("import_setup", { link, dry_run: dryRun });
export const addDomain = (name: string, domain: string) => run<null>("add_domain", { name, domain });
export const removeDomain = (name: string, domain: string) => run<null>("remove_domain", { name, domain });
export const setOptions = (name: string, prefs: Prefs) => run<null>("set_options", { name, ...prefs });
export const fileTargets = (name: string) => run<FileTarget[]>("file_targets", { name });
export const sendFile = (name: string, target: string, path: string) => run<null>("send_file", { name, target, path });
export const waitingFiles = (name: string) => run<WaitingFile[]>("waiting_files", { name });
export const receiveFile = (name: string, file: string, destDir: string) => run<{ path: string }>("receive_file", { name, file, dest_dir: destDir });
export const setUnlocked = (on: boolean) => run<null>("set_unlocked", { on });
export const setAllApps = (on: boolean) => run<null>("set_all_apps", { on });
export const start = () => backend.start();
export const stop = () => backend.stop();
export const getAlwaysOn = () => backend.getAlwaysOn();
export const setAlwaysOn = (enabled: boolean) => backend.setAlwaysOn(enabled);

/** Calls `fn` on the engine's `change` event; returns the unsubscribe function. */
export function onChange(fn: () => void): () => void {
  return usingMock ? mock.subscribe(fn) : Native.addChangeListener(fn);
}
