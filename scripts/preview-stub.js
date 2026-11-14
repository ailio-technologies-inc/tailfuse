// Stand-in for the Tauri app, used only when index.html is opened from a local dev server to look
// at the design in a browser (see main.js). Add ?empty or ?off to the address for those states, or ?shot for the
// clean, licensed state the README and tailfuse.com screenshots are taken from (scripts/screenshots.sh).
// Tracked here; copy it to app/src/preview-stub.js (git-ignored, so it never ships) to preview the UI locally.
const q = new URLSearchParams(location.search);
const shot = q.has("shot");
const state = shot ? {
  up: true,
  pac_url: "http://127.0.0.1:43100/tailfuse.pac",
  license: "licensed to you@example.com",
  licensed_to: "you@example.com",
  profiles: [
    { name: "home-lab", suffixes: [".home-lab.ts.net"], online: true, hostname: "studio-mac", ips: ["100.101.14.9"], error: "" },
    { name: "work", suffixes: [".work.ts.net", ".internal.example"], online: true, hostname: "studio-mac-work", ips: ["100.72.203.51"], error: "" },
    { name: "client-acme", suffixes: [".acme-corp.ts.net"], online: false, paused: true, hostname: "", ips: [], error: "" },
  ],
  proxy_problem: "",
  tailscale_app: null,
  cli: "/usr/local/bin/tailfuse",
  tunnels: [{ name: "prod-db", listen: "127.0.0.1:5432", target: "db.work.ts.net:5432", profile: "work" }],
} : {
  up: !q.has("off"),
  pac_url: "http://127.0.0.1:43100/tailfuse.pac",
  license: "free (2/2 profiles)",
  profiles: q.has("empty") ? [] : [
    { name: "primary", suffixes: [".tail8e2dc7.ts.net"], online: !q.has("off"), hostname: q.has("off") ? "" : "studio-mac", ips: q.has("off") ? [] : ["100.101.102.103"], error: "" },
    { name: "work", suffixes: [".work.ts.net"], online: false, hostname: "", ips: [], error: "" },
  ],
  proxy_problem: q.has("off") || q.has("empty") ? "" : "permission",
  tailscale_app: q.has("empty") ? null : { tailnet: "example.com", account: "you@example.com", same_as: "primary" },
  cli: "",
  tunnels: q.has("empty") ? [] : [{ name: "prod-db", listen: "127.0.0.1:5432", target: "db.work.ts.net:5432", profile: "work" }],
};
export async function invoke(cmd, args) {
  if (cmd === "get_state") return state;
  if (cmd === "test_host") return /work\.ts\.net$/.test(args.host) ? `${args.host} → profile "work"` : `${args.host} → not owned (would be refused)`;
  if (cmd === "get_autostart") return shot;
  if (cmd === "start") state.up = true;
  if (cmd === "stop") state.up = false;
  if (cmd === "sign_in_new_tailnet") { await new Promise((r) => setTimeout(r, 2500)); return "example"; }
  if (cmd === "set_tailnet") { const p = state.profiles.find((x) => x.name === args.name); if (p) { p.online = args.on; p.paused = !args.on; } }
  return null;
}
