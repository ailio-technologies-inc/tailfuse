// The engine's State JSON (engines/mobile/API.md).
/** A tailnet's Tailscale settings (`set_options`). */
export type Prefs = {
  exit_node?: string;
  accept_routes?: boolean;
  shields_up?: boolean;
};

export type Profile = {
  name: string;
  account: string;
  tailnet: string;
  suffixes: string[];
  hostname: string;
  ips: string[];
  online: boolean;
  paused: boolean;
  error: string;
  auth_url: string;
  /** A sign-in is known to be required (not merely off or still connecting). */
  needs_login: boolean;
  /** Tailfuse is on and the node is starting; no sign-in needed. */
  connecting: boolean;
  /** Saved Tailscale settings; absent from older engines. */
  prefs?: Prefs;
};

export type Tunnel = { name: string; listen: string; target: string; profile: string };

export type State = {
  up: boolean;
  version?: string;
  proxy: string;
  pac_url: string;
  unlocked?: boolean;
  free_profiles?: number;
  match_domains: string[];
  /** All-apps mode is the saved setting (on by default). */
  all_apps?: boolean;
  /** The engine is reading the tunnel (all-apps mode is in effect). */
  all_apps_active?: boolean;
  /** Why all-apps mode fell back to proxy mode this session; "" when it did not. */
  all_apps_error?: string;
  profiles: Profile[];
  tunnels: Tunnel[];
};

export type TestHostResult = { host: string; owner: string | null; via?: "address"; suggest_domain?: string };

export type SyncResult = {
  from: string;
  added: string[];
  already: string[];
  needs_unlock: string[];
  tunnels_added: string[];
  skipped: { name: string; reason: string }[];
};

/** A device that can be sent files (Taildrop); `id` is `send_file`'s target. */
export type FileTarget = { name: string; dns_name: string; id: string };
export type WaitingFile = { name: string; size: number };

export type Device = { name: string; dns_name: string; ips: string[]; os: string; online: boolean; self?: boolean };

/** One page of `devices`; totals count every device on the tailnet, this one included. */
export type DevicePage = { devices: Device[]; total: number; online: number; matched: number };
