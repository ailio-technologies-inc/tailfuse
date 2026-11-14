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
    return ["Tailscale isn't installed", "Tailfuse runs on Tailscale's own software. Install Tailscale from tailscale.com/download, then try again."];
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
    return ["Add a tailnet first", "Use Sign in with Tailscale, then turn Tailfuse on."];
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
  const waiting = s.profiles.filter((p) => !p.online && !p.error);
  const broken = s.profiles.filter((p) => p.error);
  if (broken.length) return `${broken[0].name} has a problem. See the note beside it.`;
  if (waiting.length) return `Sign in to ${waiting.map((p) => p.name).join(" and ")} to bring ${waiting.length === 1 ? "it" : "them"} online.`;
  if (s.proxy_problem) return `${plural(online, "tailnet")} online. One more step before your apps can reach ${online === 1 ? "it" : "them"}.`;
  return `${plural(online, "tailnet")} online. Open any of their addresses as usual.`;
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
  b.innerHTML = armed
    ? '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 12l5 5 9-10"/></svg>'
    : '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3"/></svg>';
  return b;
}

function profileEntry(p, up) {
  const [label, tone] = tailnetState(p, up);
  const ips = p.ips.filter(Boolean);
  // Hover or focus shows these in a card that floats over the list, so nothing below moves.
  const details = [p.hostname && ["This Mac there", p.hostname], ...ips.map((ip) => [ip.includes(":") ? "IPv6" : "IPv4", ip]), p.account && ["Account", p.account]].filter(Boolean);
  const needsSignIn = up && !p.online && !p.paused && !p.error;
  return h("li", { class: "entry entry--tailnet", tabIndex: 0 },
    h("span", { class: `dot${p.online ? " on" : ""}${p.error ? " bad" : ""}` }),
    h("div", { class: "entry-main" },
      h("div", { class: "entry-line" },
        h("strong", null, p.name),
        p.account && h("span", { class: "muted entry-account" }, p.account),
      ),
      h("span", { class: "mono muted" }, p.suffixes.map((x) => `*${x}`).join("  ")),
      p.error && h("span", { class: "muted bad" }, p.error),
      tailscaleApp?.same_as === p.name && h("span", { class: "ts-badge" },
        h("span", { class: "dot on" }), "Also connected in the Tailscale app ",
        h("button", { class: "link-btn", type: "button", id: "ts-app-off", title: "Disconnect the Tailscale app; it stays signed in" }, "Disconnect it")),
    ),
    h("div", { class: "entry-actions" },
      h("span", { class: `state ${tone}` }, label),
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

function render(s) {
  lastState = s;
  $("#power").checked = s.up;
  $("#power-label").textContent = s.up ? "On" : "Off";
  $("#power-label").classList.toggle("on", s.up);
  $("#status").textContent = statusLine(s);
  $("#license").textContent = s.license;

  // Browsers only reach the tailnets once the system points them at Tailfuse.
  $("#proxy-note").hidden = !s.proxy_problem;
  const needsYou = s.proxy_problem === "permission";
  $("#proxy-fix").hidden = !needsYou;
  $("#proxy-why").textContent = needsYou
    ? "Your tailnets are online, but your Mac isn't sending their addresses through Tailfuse yet. Allow Tailfuse to set the system proxy; macOS asks for your password once."
    : `${sentence(s.proxy_problem || "")} You can still set it by hand: see Advanced, under Settings.`;
  $("#pac-url").textContent = s.pac_url;

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
  $("#profile-hint").textContent = count === 0 ? "" : count <= 2 ? `${count} of 2 free tailnets used` : `${count} tailnets, licensed`;
  if (!signingIn) {
    const atLimit = count >= 2 && !/licensed|valid/i.test(s.license || "");
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

  $("#activate").addEventListener("submit", async (e) => {
    e.preventDefault();
    const msg = $("#license-msg");
    try {
      msg.textContent = sentence(await invoke("activate_license", { key: $("#license-key").value.trim() }));
      $("#license-key").value = "";
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
