// The engine's State JSON (engines/mobile/API.md).
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
  profiles: Profile[];
  tunnels: Tunnel[];
};

export type TestHostResult = { host: string; owner: string | null };

export type Device = { name: string; dns_name: string; ips: string[]; os: string; online: boolean };
