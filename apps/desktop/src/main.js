// tailfuse window. No bundler: the app exposes Tauri on window.__TAURI__ (withGlobalTauri in
// tauri.conf.json). Opened from a local dev server instead (a design preview in a browser), it
// falls back to preview-stub.js; inside the app that fallback is never taken.

// The menu-bar panel is a second webview that loads this same index; send it to
// its own page before anything else runs.
if (window.__TAURI__?.webviewWindow?.getCurrentWebviewWindow) {
  const me = await window.__TAURI__.webviewWindow.getCurrentWebviewWindow();
  if (me.label === "panel") location.replace("panel.html");
}

const tauri = window.__TAURI__?.core;
const isPreview = !tauri && location.protocol === "http:" && location.port !== "";
const invoke = tauri ? tauri.invoke : isPreview ? (await import("./preview-stub.js")).invoke : null;

const $ = (sel) => document.querySelector(sel);

// Builds one element: h("span", { class: "muted" }, "text", childNode, …). Text is always set as
// text, never parsed as markup, so a tailnet called "<b>" shows as exactly that.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs ?? {})) {
    if (value === false || value == null) continue;
    if (key === "class") el.className = value;
    else if (key.startsWith("data-")) el.setAttribute(key, value);
    else el[key] = value;
  }
  for (const child of children.flat()) {
    if (child === false || child == null || child === "") continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return el;
}

// App messages arrive lower-case and unpunctuated ("imported …", "activation failed: …").
function sentence(text) {
  const t = String(text?.message ?? text).trim().replace(/^Error:\s*/, "");
  if (!t) return "";
  const cased = t[0].toUpperCase() + t.slice(1);
  return /[.!?]$/.test(cased) ? cased : `${cased}.`;
}

// Turn the app's raw messages into something a person can act on.
function friendly(err) {
  const raw = String(err?.message ?? err);
  if (/tailscaled not found|tailscale CLI not found/i.test(raw)) {
    return ["Tailfuse's runtime is missing", "Reinstall Tailfuse to restore its bundled Tailscale runtime, then try again. Tailscale itself does not need to be installed."];
  }
  if (/requires a license/i.test(raw)) {
    return ["That needs a license key", "Two tailnets are free. Get a key at tailfuse.com (the donation is optional) and paste it under License."];
  }
  if (/address already in use|router:/i.test(raw)) {
    return ["Tailfuse's port is busy", "Another program, or a second copy of Tailfuse, is using port 43100. Quit it and turn Tailfuse on again."];
  }
  if (/cancelled the password prompt/i.test(raw)) {
    return ["Nothing was changed", "You closed the password prompt. Try again when you're ready."];
  }
  if (/not finished in time/i.test(raw)) {
    return ["The sign-in ran out of time", "Start it again and finish in the browser page within a few minutes."];
  }
  if (/no profiles defined/i.test(raw)) {
    return ["Add a tailnet first", "Sign in with Tailscale under Your tailnets, then turn Tailfuse on."];
  }
  return ["Something went wrong", sentence(raw)];
}

// One place for things that went wrong: in the window, and as a system notification when the
// window is not the one in front.
function showError(err) {
  const [title, text] = friendly(err);
  // A native alert while the window is in front; a system notification when it isn't.
  const cmd = document.hasFocus() ? "alert_user" : "notify_user";
  invoke(cmd, { title, body: text }).catch(() => {
    $("#error-title").textContent = title;
    $("#error-text").textContent = text;
    $("#error").hidden = false;
  });
}

async function call(cmd, args) {
  try {
    return await invoke(cmd, args);
  } catch (err) {
    showError(err);
    throw err;
  }
}

const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

// The one line under the title bar: where things stand and what to do next.
function statusLine(s) {
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
  if (s.proxy_problem) return `${plural(online, "tailnet")} online. One more step before your apps can reach ${online === 1 ? "it" : "them"}.`;
  if (online === 0) return `Every tailnet is switched off. Turn one on below.`;
  return `${plural(online, "tailnet")} online.${offNote} Open any of their addresses as usual.`;
}

let lastState = null;
let cliBusy = false;
let signingIn = false;
let tailscaleApp = null;
const pendingRemove = new Map(); // "profile:work" -> timer, for the two-step Remove

function removeButton(kind, name) {
  const id = `${kind}:${name}`;
  const armed = pendingRemove.has(id);
  return h("button", { class: `btn btn--secondary btn--sm${armed ? " is-armed" : ""}`, type: "button", "data-remove": id }, armed ? "Remove for good" : "Remove");
}

function tailnetState(p, up) {
  if (p.error) return ["Problem", "pill-bad"];
  if (p.online) return ["Connected", "pill-on"];
  if (!up) return ["Off", ""];
  if (p.paused) return ["Disconnected", ""];
  return ["Sign-in needed", "pill-wait"];
}

// A small remove control for a tailnet row; asks twice like the Remove button.
function removeIcon(name) {
  const id = `profile:${name}`;
  const armed = pendingRemove.has(id);
  const b = h("button", { class: `icon-btn${armed ? " is-armed" : ""}`, type: "button", "data-remove": id, title: armed ? "Click again to remove" : `Remove ${name}`, "aria-label": armed ? `Confirm removing ${name}` : `Remove ${name}` });
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  for (const [key, value] of Object.entries({
    viewBox: "0 0 24 24", width: "16", height: "16", fill: "none",
    stroke: "currentColor", "stroke-width": armed ? "2" : "1.9",
    "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true",
  })) svg.setAttribute(key, value);
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", armed
    ? "M5 12l5 5 9-10"
    : "M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3");
  svg.append(path);
  b.replaceChildren(svg);
  return b;
}

function profileEntry(p, up) {
  const [label, tone] = tailnetState(p, up);
  const ips = p.ips.filter(Boolean);
  // Hover or focus shows these in a card that floats over the list, so nothing below moves.
  const details = [p.hostname && ["This Mac's name", p.hostname], ...ips.map((ip) => [ip.includes(":") ? "IPv6" : "IPv4", ip]), p.account && ["Name in Tailfuse", p.name]].filter(Boolean);
  const needsSignIn = up && !p.online && !p.paused && !p.error;
  return h("li", { class: "entry entry--tailnet", tabIndex: 0 },
    h("span", { class: `dot${p.online ? " on" : ""}${p.error ? " bad" : ""}` }),
    h("div", { class: "entry-main" },
      h("div", { class: "entry-line" },
        // Shown by the account it signed in with, like the menu-bar panel; the profile name follows.
        h("strong", { title: p.account || p.name }, p.account || p.name),
        p.account && h("span", { class: "muted entry-account" }, p.name),
      ),
      h("span", { class: "mono muted" }, p.suffixes.map((x) => `*${x}`).join("  ")),
      p.error && h("span", { class: "muted bad" }, p.error),
      tailscaleApp?.same_as === p.name && h("span", { class: "ts-badge" },
        h("span", { class: "dot on" }), "Also connected in the Tailscale app ",
        h("button", { class: "link-btn", type: "button", id: "ts-app-off", title: "Disconnect the Tailscale app; it stays signed in" }, "Disconnect it")),
    ),
    h("div", { class: "entry-actions" },
      h("span", { class: `state ${tone}` }, label),
      h("button", { class: "btn btn--secondary btn--sm", type: "button", "data-options": p.name }, "Options"),
      needsSignIn
        ? h("button", { class: "btn btn--primary btn--sm", type: "button", "data-login": p.name }, "Sign in")
        : h("label", { class: "switch switch--sm", title: p.online ? `Disconnect ${p.name}` : `Connect ${p.name}` },
            h("input", { type: "checkbox", checked: Boolean(p.online), "data-tailnet": p.name, "aria-label": `${p.name} connected` }),
            h("span", { class: "slider" }),
          ),
      h("span", { class: "entry-hover" }, removeIcon(p.name)),
    ),
    details.length > 0 && h("dl", { class: "entry-pop", role: "tooltip" },
      ...details.flatMap(([k, v]) => [h("dt", null, k), h("dd", { class: k.startsWith("IP") ? "mono" : "" }, v)]),
    ),
  );
}

function tunnelEntry(t) {
  return h("li", { class: "entry" },
    h("span", { class: "dot tunnel" }),
    h("div", { class: "entry-main" },
      h("strong", null, t.name),
      h("span", { class: "muted" }, h("span", { class: "mono" }, t.listen), " on this computer leads to ", h("span", { class: "mono" }, t.target)),
      h("span", { class: "muted" }, `through ${t.profile}`),
    ),
    h("div", { class: "entry-actions" }, removeButton("tunnel", t.name)),
  );
}

// Tailnet options: what `tailscale set`, `serve` and `funnel` manage, saved to the config and
// applied to the running node. Lists are typed as comma-separated text.
let optionsFor = null;
const csv = (v) => v.split(",").map((x) => x.trim()).filter(Boolean);

function parseServe(text) {
  return csv(text).map((spec) => {
    const funnel = spec.startsWith("funnel:");
    const [port, ...rest] = (funnel ? spec.slice(7) : spec).split("=");
    const target = rest.join("=");
    if (!target || !Number.isInteger(Number(port))) throw new Error(`"${spec}" should look like 443=3000`);
    return { port: Number(port), target, funnel };
  });
}

async function openOptions(name) {
  const o = await call("get_profile_options", { name }).catch(() => null);
  if (!o) return;
  optionsFor = name;
  $("#options-title").textContent = `Options for ${name}`;
  $("#o-hostname").value = o.hostname;
  $("#o-exit").value = o.exit_node;
  $("#o-routes").value = o.advertise_routes.join(", ");
  $("#o-serve").value = o.serve.map((e) => `${e.funnel ? "funnel:" : ""}${e.port}=${e.target}`).join(", ");
  $("#o-accept").checked = o.accept_routes;
  $("#o-lan").checked = o.exit_node_allow_lan_access;
  $("#o-adv-exit").checked = o.advertise_exit_node;
  $("#o-ssh").checked = o.ssh;
  $("#o-shields").checked = o.shields_up;
  $("#o-out").hidden = true;
  $("#options").showModal();
}

async function saveOptions() {
  try {
    const options = {
      accept_routes: $("#o-accept").checked,
      exit_node: $("#o-exit").value,
      hostname: $("#o-hostname").value,
      advertise_routes: csv($("#o-routes").value),
      advertise_exit_node: $("#o-adv-exit").checked,
      exit_node_allow_lan_access: $("#o-lan").checked,
      ssh: $("#o-ssh").checked,
      shields_up: $("#o-shields").checked,
      serve: parseServe($("#o-serve").value),
    };
    await call("set_profile_options", { name: optionsFor, options });
    $("#options").close();
  } catch (err) {
    showError(err);
  }
}

async function taildrop(cmd, args) {
  const out = $("#o-out");
  out.hidden = false;
  out.textContent = "Working…";
  try {
    out.textContent = await invoke(cmd, { name: optionsFor, ...args });
  } catch (err) {
    out.textContent = sentence(err);
  }
}

async function runTailscale() {
  const out = $("#o-out");
  const args = $("#o-cmd").value.trim().split(/\s+/).filter(Boolean);
  if (!args.length) return;
  out.hidden = false;
  out.textContent = "Running…";
  try {
    out.textContent = (await invoke("tailscale_run", { name: optionsFor, args })).trim() || "Done.";
  } catch (err) {
    out.textContent = sentence(err);
  }
}

let licenseFormOpen = false;

/** Settings > Every app: all-apps mode on the Mac (src-tauri/src/allapps.rs). */
function renderAllApps(a) {
  if (!a) return;
  $("#all-apps-row").hidden = !a.available;
  if (allAppsBusy) return;
  $("#all-apps").checked = a.on;
  const text = $("#all-apps-text");
  if (a.on && a.tunnel !== "connected" && a.tunnel !== "none") {
    text.textContent = `Every app reaches your tailnets. The tunnel is ${a.tunnel}.`;
  } else if (a.on) {
    text.textContent = "Every app on this Mac reaches your tailnets: ssh, database tools, the terminal and browsers.";
  }
}

let allAppsBusy = false;

function render(s) {
  renderAllApps(s.all_apps);
  lastState = s;
  if (s.version) $("#version").textContent = s.version;
  $("#power").checked = s.up;
  $("#power-label").textContent = s.up ? "On" : "Off";
  $("#power-label").classList.toggle("on", s.up);
  $("#status").textContent = statusLine(s);
  $("#license").textContent = s.license;
  const licensed = !!s.licensed_to;
  $("#licensed").hidden = !licensed;
  $("#unlicensed").hidden = licensed;
  if (licensed) $("#licensed-to").textContent = s.licensed_to;
  // Licensed: the key form stays out of sight until "Use another key".
  if (licensed && !licenseFormOpen) $("#activate").hidden = true;
  if (!licensed) $("#activate").hidden = false;

  // Browsers only reach the tailnets once the system points them at Tailfuse.
  $("#proxy-note").hidden = !s.proxy_problem;
  const needsYou = s.proxy_problem === "permission";
  $("#proxy-fix").hidden = !needsYou;
  $("#proxy-why").textContent = needsYou
    ? "Your tailnets are online, but your Mac isn't sending their addresses through Tailfuse yet. Allow Tailfuse to set the system proxy; macOS asks for your password once."
    : `${sentence(s.proxy_problem || "")} You can still set it by hand: see System proxy, under Settings.`;
  $("#pac-url").textContent = s.pac_url;
  const hasTailnets = s.profiles.length > 0;
  $("#phone-show").disabled = !hasTailnets;
  $("#phone-text").textContent = hasTailnets
    ? "Show a code to scan with Tailfuse on your iPhone or Android phone. It brings your tailnets across; you sign in to each one on the phone."
    : "Add a tailnet first.";

  // The official Tailscale app: shown on the tailnet it shares with Tailfuse; a separate
  // notice only when it is on a tailnet Tailfuse doesn't have.
  const app = s.tailscale_app;
  tailscaleApp = app;
  $("#ts-app").hidden = !app || Boolean(app.same_as);
  if (app && !app.same_as) {
    $("#ts-app-text").textContent = `It is on ${[app.tailnet, app.account && `as ${app.account}`].filter(Boolean).join(" ") || "another tailnet"}, side by side with Tailfuse.`;
  }

  $("#cli-install").textContent = s.cli ? "Reinstall" : "Install";
  if (!cliBusy && s.cli) $("#cli-text").textContent = `Installed at ${s.cli}.`;

  const count = s.profiles.length;
  $("#profile-hint").textContent = count === 0 ? "" : s.licensed_to ? `${count} ${count === 1 ? "tailnet" : "tailnets"}, licensed` : count <= 2 ? `${count} of 2 free tailnets used` : `${count} tailnets`;
  if (!signingIn) {
    const atLimit = count >= 2 && !s.licensed_to;
    $("#signin-title").textContent = count === 0 ? "Add your first tailnet" : "Add another tailnet";
    $("#signin-sub").textContent = atLimit
      ? "Sign in with Tailscale in your browser. A third tailnet needs a license key (by optional donation)."
      : "Sign in with Tailscale in your browser. The tailnet, its name and its account fill themselves in.";
  }
  $("#profiles").replaceChildren(...s.profiles.map((p) => profileEntry(p, s.up)));
  $("#tunnels").replaceChildren(...s.tunnels.map(tunnelEntry));

  // The tailnet choice in the local-port form follows the tailnets that exist.
  const select = $("#t-profile");
  const chosen = select.value;
  select.replaceChildren(...s.profiles.map((p) => h("option", null, p.name)));
  if (s.profiles.some((p) => p.name === chosen)) select.value = chosen;
  $("#show-add-tunnel").disabled = count === 0;
}

// The code the phone scans. The SVG comes from our own Rust, but it is still parsed and checked
// (only an <svg> root is taken, nothing is concatenated into markup); the text around it is set
// with textContent, so a tailnet's name never reaches innerHTML.
let phoneLink = "";
async function showPhoneCode() {
  const r = await invoke("phone_sync");
  const doc = new DOMParser().parseFromString(r.svg, "image/svg+xml");
  const svg = doc.documentElement;
  if (svg.localName !== "svg" || doc.querySelector("parsererror")) throw new Error("The code could not be drawn.");
  svg.querySelectorAll("script, foreignObject").forEach((n) => n.remove());
  $("#phone-qr").replaceChildren(document.importNode(svg, true));
  phoneLink = r.link;
  let line = plural(r.profiles, "tailnet");
  if (r.tunnels) line += ` and ${plural(r.tunnels, "local port")}`;
  line += ". No passwords or keys are in this code.";
  if (r.tunnels_dropped) line += " Your local ports were left out: there are too many for one code.";
  $("#phone-carries").textContent = line;
  $("#phone-copy").textContent = "Copy link";
  const dialog = $("#phone");
  if (!dialog.open) dialog.showModal();
}

// The system proxy needs an administrator. Explain first in the window; only after OK does
// macOS show its own password prompt. Asked once per launch on its own; the notice's Allow
// button asks again whenever.
let askedThisLaunch = false;
async function askPermission() {
  const dialog = $("#permission");
  if (dialog.open) return;
  dialog.returnValue = "";
  dialog.showModal();
  const answer = await new Promise((resolve) => dialog.addEventListener("close", () => resolve(dialog.returnValue), { once: true }));
  if (answer !== "ok") return;
  $("#proxy-fix").disabled = true;
  try {
    await call("connect_browsers");
  } catch {
    // the error line already says why
  }
  $("#proxy-fix").disabled = false;
  refresh();
}

function maybeAskPermission(s) {
  if (s.up && s.proxy_problem === "permission" && !askedThisLaunch) {
    askedThisLaunch = true;
    askPermission();
  }
}

async function refresh() {
  try {
    const s = await invoke("get_state");
    render(s);
    maybeAskPermission(s);
  } catch (err) {
    showError(err);
  }
}

function toggleForm(id, open) {
  const form = $(`#${id}`);
  const button = $(`[aria-controls="${id}"]`);
  form.hidden = !open;
  button.setAttribute("aria-expanded", String(open));
  button.hidden = open;
  if (open) form.querySelector("input, select")?.focus();
}

if (!invoke) {
  $("#status").textContent = "This window could not reach the Tailfuse app. Quit Tailfuse and open it again.";
} else {
  // Quick connect/disconnect for one tailnet.
  document.addEventListener("change", async (e) => {
    const name = e.target.dataset?.tailnet;
    if (!name) return;
    e.target.disabled = true;
    await call("set_tailnet", { name, on: e.target.checked }).catch(() => {});
    e.target.disabled = false;
    refresh();
  });

  $("#power").addEventListener("change", async (e) => {
    e.target.disabled = true;
    try {
      await call(e.target.checked ? "start" : "stop");
    } catch {
      // the error line already says why
    }
    e.target.disabled = false;
    refresh();
  });

  document.addEventListener("click", async (e) => {
    const target = e.target.closest("button");
    if (!target) return;

    if (target.dataset.close) return toggleForm(target.dataset.close, false);
    if (target.id === "error-close") return void ($("#error").hidden = true);

    if (target.id === "proxy-fix") return askPermission();
    if (target.id === "proxy-remove") {
      target.disabled = true;
      await call("disconnect_browsers").catch(() => {});
      target.disabled = false;
      return refresh();
    }

    if (target.id === "cli-install") {
      cliBusy = true;
      target.disabled = true;
      $("#cli-text").textContent = "Downloading the command and checking it.";
      try {
        $("#cli-text").textContent = sentence(await invoke("install_cli"));
      } catch (err) {
        $("#cli-text").textContent = friendly(err)[1];
      }
      target.disabled = false;
      setTimeout(() => (cliBusy = false), 20000);
      return refresh();
    }
    if (target.id === "show-add-tunnel") return toggleForm("add-tunnel", true);

    if (target.dataset.options) return openOptions(target.dataset.options);

    if (target.dataset.login) {
      await call("login", { name: target.dataset.login }).catch(() => {});
      return refresh();
    }

    // Remove asks twice: the first click arms the button for a few seconds.
    const id = target.dataset.remove;
    if (id) {
      if (!pendingRemove.has(id)) {
        pendingRemove.set(id, setTimeout(() => {
          pendingRemove.delete(id);
          if (lastState) render(lastState);
        }, 4000));
        return render(lastState);
      }
      clearTimeout(pendingRemove.get(id));
      pendingRemove.delete(id);
      const [kind, ...rest] = id.split(":");
      const name = rest.join(":");
      await call(kind === "profile" ? "remove_profile" : "remove_tunnel", { name }).catch(() => {});
      return refresh();
    }

    if (target.id === "phone-show") {
      target.disabled = true;
      try {
        await showPhoneCode();
      } catch (err) {
        $("#phone-text").textContent = friendly(err)[1];
      }
      target.disabled = !lastState?.profiles.length;
      return;
    }

    if (target.id === "phone-copy") {
      navigator.clipboard.writeText(phoneLink).then(() => {
        target.textContent = "Copied";
        setTimeout(() => (target.textContent = "Copy link"), 1500);
      }, () => {});
      return;
    }

    if (target.id === "mcp-copy") {
      navigator.clipboard.writeText("claude mcp add tailfuse -- tailfuse mcp").then(() => {
        target.textContent = "Copied";
        setTimeout(() => (target.textContent = "Copy setup"), 1500);
      }, () => {});
      return;
    }

    if (target.id === "pac-copy") {
      navigator.clipboard.writeText($("#pac-url").textContent).then(() => {
        target.textContent = "Copied";
        setTimeout(() => (target.textContent = "Copy address"), 1500);
      }, () => {});
      return;
    }

    if (target.id === "ts-app-off") {
      target.disabled = true;
      await call("disconnect_tailscale_app").catch(() => {});
      target.disabled = false;
      return refresh();
    }

    if (target.id === "get-key") {
      call("plugin:opener|open_url", { url: "https://tailfuse.com/#pricing" }).catch(() => {});
    }
  });

  // About: links open in the browser, not in the app's window.
  document.addEventListener("click", (e) => {
    const a = e.target.closest?.("a[data-external]");
    if (!a) return;
    e.preventDefault();
    call("plugin:opener|open_url", { url: a.href }).catch(() => {});
  });

  $("#update-check").addEventListener("click", async (e) => {
    const b = e.currentTarget;
    b.disabled = true;
    $("#update-text").textContent = "Checking for a newer version…";
    try {
      $("#update-text").textContent = await call("check_updates");
    } catch (err) {
      $("#update-text").textContent = `Could not check for updates: ${err}`;
    }
    b.disabled = false;
  });

  // Add a tailnet the way the Tailscale app does: the browser sign-in tells Tailfuse which
  // tailnet it is, so nothing has to be typed. The call returns once the sign-in is finished.
  $("#signin-new").addEventListener("click", async () => {
    const button = $("#signin-new");
    const msg = $("#signin-msg");
    signingIn = true;
    button.disabled = true;
    button.classList.add("is-busy");
    $("#signin-title").textContent = "Waiting for your browser";
    $("#signin-sub").textContent = "Finish signing in on the Tailscale page that just opened. This window updates when you're done.";
    msg.textContent = "";
    try {
      const name = await invoke("sign_in_new_tailnet");
      msg.textContent = `${name} is signed in and coming online.`;
    } catch (err) {
      showError(err);
    }
    signingIn = false;
    button.disabled = false;
    button.classList.remove("is-busy");
    refresh();
  });

  $("#add-tunnel").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await call("add_tunnel", {
        name: $("#t-name").value.trim(),
        listen: $("#t-listen").value.trim(),
        target: $("#t-target").value.trim(),
        profile: $("#t-profile").value,
      });
      e.target.reset();
      toggleForm("add-tunnel", false);
    } catch {
      // the error line already says why
    }
    refresh();
  });

  $("#change-key").addEventListener("click", () => {
    licenseFormOpen = !licenseFormOpen;
    $("#activate").hidden = !licenseFormOpen;
    $("#change-key").setAttribute("aria-expanded", String(licenseFormOpen));
    $("#change-key").textContent = licenseFormOpen ? "Cancel" : "Use another key";
    if (licenseFormOpen) $("#license-key").focus();
  });

  $("#activate").addEventListener("submit", async (e) => {
    e.preventDefault();
    const msg = $("#license-msg");
    try {
      msg.textContent = sentence(await invoke("activate_license", { key: $("#license-key").value.trim() }));
      $("#license-key").value = "";
      licenseFormOpen = false;
      $("#change-key").setAttribute("aria-expanded", "false");
      $("#change-key").textContent = "Use another key";
    } catch (err) {
      msg.textContent = sentence(err);
    }
    refresh();
  });

  // The app answers `host → profile "name"` or `host → not owned (would be refused)`.
  $("#test-host").addEventListener("submit", async (e) => {
    e.preventDefault();
    const host = $("#test-input").value.trim();
    const out = $("#test-result");
    try {
      const answer = await invoke("test_host", { host });
      const owner = String(answer).match(/profile "([^"]+)"/);
      out.textContent = owner
        ? `${host} goes to ${owner[1]}.`
        : `${host} is on none of your tailnets. Tailfuse leaves it alone, and your apps reach it directly.`;
    } catch (err) {
      out.textContent = sentence(err);
    }
  });

  $("#o-save").addEventListener("click", saveOptions);
  $("#o-run").addEventListener("click", runTailscale);
  $("#o-send").addEventListener("click", () => taildrop("taildrop_send", { device: $("#o-drop").value }));
  $("#o-cert").addEventListener("click", () => taildrop("tailscale_cert", {}));
  $("#o-lock").addEventListener("click", () => {
    $("#o-cmd").value = "lock status";
    runTailscale();
  });
  $("#o-receive").addEventListener("click", () => taildrop("taildrop_receive", {}));
  $("#o-cmd").addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      runTailscale();
    }
  });

  $("#all-apps").addEventListener("change", async (e) => {
    const on = e.target.checked;
    allAppsBusy = true;
    e.target.disabled = true;
    $("#all-apps-text").textContent = on
      ? "Starting Tailfuse's tunnel. If macOS asks, allow it in System Settings, then sign in to each tailnet in your browser."
      : "Handing your tailnets back to Tailfuse's own connections…";
    try {
      await call("set_all_apps", { on });
    } catch {
      e.target.checked = !on;
    } finally {
      allAppsBusy = false;
      e.target.disabled = false;
      refresh();
    }
  });

  $("#autostart").addEventListener("change", async (e) => {
    try {
      await call("set_autostart", { enabled: e.target.checked });
    } catch {
      e.target.checked = !e.target.checked;
    }
  });

  invoke("get_autostart")
    .then((v) => ($("#autostart").checked = v))
    .catch(() => {});

  refresh();
  setInterval(refresh, 3000);
}
