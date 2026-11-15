// Stand-in for the Tauri app, used only when index.html is opened from a local dev server to look
// at the design in a browser (see main.js). Add ?empty or ?off to the address for those states, or ?shot for the
// clean, licensed state the README and tailfuse.com screenshots are taken from (scripts/screenshots.sh).
// Tracked here; copy it to apps/desktop/src/preview-stub.js (git-ignored, so it never ships) to preview the UI locally.
const q = new URLSearchParams(location.search);
const shot = q.has("shot");
const state = shot ? {
  up: true,
  pac_url: "http://127.0.0.1:43100/tailfuse.pac",
  license: "licensed to you@example.com",
  licensed_to: "you@example.com",
  profiles: [
    { name: "home-lab", account: "you@gmail.com", suffixes: [".home-lab.ts.net"], online: true, hostname: "studio-mac", ips: ["100.101.14.9"], error: "" },
    { name: "work", account: "you@example.com", suffixes: [".work.ts.net", ".internal.example"], online: true, hostname: "studio-mac-work", ips: ["100.72.203.51"], error: "" },
    { name: "client-acme", account: "you@acme-corp.com", suffixes: [".acme-corp.ts.net"], online: false, paused: true, hostname: "", ips: [], error: "" },
  ],
  proxy_problem: "",
  tailscale_app: null,
  cli: "/usr/local/bin/tailfuse",
  version: "0.1.24",
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
  if (cmd === "phone_sync") {
    // A real code (the test vector's link), pre-rendered, so the dialog looks right in a browser.
    const n = 29, cells = [];
    let seed = 7;
    const finder = (x, y) => [[0, 0], [n - 7, 0], [0, n - 7]].some(([fx, fy]) => x >= fx && x < fx + 7 && y >= fy && y < fy + 7);
    const ring = (x, y) => { for (const [fx, fy] of [[0, 0], [n - 7, 0], [0, n - 7]]) { const dx = x - fx, dy = y - fy; if (dx >= 0 && dx < 7 && dy >= 0 && dy < 7) return dx === 0 || dx === 6 || dy === 0 || dy === 6 || (dx > 1 && dx < 5 && dy > 1 && dy < 5); } return false; };
    for (let y = 0; y < n; y++) for (let x = 0; x < n; x++) { seed = (seed * 1103515245 + 12345) & 0x7fffffff; if (finder(x, y) ? ring(x, y) : (seed >> 16) & 1) cells.push(`M${x + 4},${y + 4}h1v1h-1z`); }
    const size = n + 8;
    const svg = `<?xml version="1.0" standalone="yes"?><svg xmlns="http://www.w3.org/2000/svg" version="1.1" width="264" height="264" viewBox="0 0 ${size} ${size}" shape-rendering="crispEdges"><rect width="${size}" height="${size}" fill="#ffffff"/><path d="${cells.join("")}" fill="#18191b"/></svg>`;
    if (!state.profiles.length) throw new Error("there are no tailnets to sync yet; add one first");
    return { svg, link: "tailfuse://sync?v=1&d=preview", profiles: state.profiles.length, tunnels: state.tunnels.length, tunnels_dropped: false };
  }
  if (cmd === "test_host") return /work\.ts\.net$/.test(args.host) ? `${args.host} → profile "work"` : `${args.host} → not owned (would be refused)`;
  if (cmd === "get_autostart") return shot;
  if (cmd === "check_updates") return "You have the latest version (0.1.24).";
  if (cmd === "start") state.up = true;
  if (cmd === "stop") state.up = false;
  if (cmd === "sign_in_new_tailnet") { await new Promise((r) => setTimeout(r, 2500)); return "example"; }
  if (cmd === "set_tailnet") { const p = state.profiles.find((x) => x.name === args.name); if (p) { p.online = args.on; p.paused = !args.on; } }
  return null;
}
