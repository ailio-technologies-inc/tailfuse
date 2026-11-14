// tailfuse window. No bundler: the app exposes Tauri on window.__TAURI__ (withGlobalTauri in
// tauri.conf.json). Opened from a local dev server instead (a design preview in a browser), it
// falls back to preview-stub.js; inside the app that fallback is never taken.
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
let errorTimer = 0;
function showError(err) {
  const [title, text] = friendly(err);
  $("#error-title").textContent = title;
  $("#error-text").textContent = text;
  $("#error").hidden = false;
  clearTimeout(errorTimer);
  errorTimer = setTimeout(() => ($("#error").hidden = true), 15000);
  if (!document.hasFocus()) invoke("notify_user", { title, body: text }).catch(() => {});
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
  if (s.proxy_problem) return `${plural(online, "tailnet")} online. One more step before your browser can reach ${online === 1 ? "it" : "them"}.`;
  return `${plural(online, "tailnet")} online. Open any of their addresses in your browser as usual.`;
}

let lastState = null;
let cliBusy = false;
const pendingRemove = new Map(); // "profile:work" -> timer, for the two-step Remove

function removeButton(kind, name) {
  const id = `${kind}:${name}`;
  const armed = pendingRemove.has(id);
  return h("button", { class: `btn btn--secondary btn--sm${armed ? " is-armed" : ""}`, type: "button", "data-remove": id }, armed ? "Remove for good" : "Remove");
}

function profileEntry(p, up) {
  const state = p.error ? "Problem" : p.online ? "Online" : up ? "Sign-in needed" : "Off";
  const where = [p.hostname, ...p.ips].filter(Boolean).join(" · ");
  return h("li", { class: "entry" },
    h("span", { class: `dot${p.online ? " on" : ""}${p.error ? " bad" : ""}` }),
    h("div", { class: "entry-main" },
      h("strong", null, p.name),
      h("span", { class: "muted" }, "Addresses ending in ", h("span", { class: "mono" }, p.suffixes.join(", "))),
      p.account && h("span", { class: "muted" }, `Signed in as ${p.account}`),
      where && h("span", { class: "muted" }, `This computer there: ${where}`),
      p.error && h("span", { class: "muted bad" }, p.error),
    ),
    h("div", { class: "entry-actions" },
      h("span", { class: `pill${p.online ? " pill-on" : ""}${p.error ? " pill-bad" : ""}` }, state),
      !p.online && up && h("button", { class: "btn btn--primary btn--sm", type: "button", "data-login": p.name }, "Sign in"),
      removeButton("profile", p.name),
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
    ? "Your tailnets are online, but your browsers don't know to use Tailfuse yet. macOS asks for your password once to allow it."
    : `${sentence(s.proxy_problem || "")} You can still set it by hand: see Advanced, under Settings.`;
  $("#pac-url").textContent = s.pac_url;

  // The official Tailscale app, while it is connected alongside Tailfuse.
  const app = s.tailscale_app;
  $("#ts-app").hidden = !app;
  if (app) {
    const who = [app.tailnet, app.account && `as ${app.account}`].filter(Boolean).join(" ");
    $("#ts-app-text").textContent = app.same_as
      ? `It is on ${who}, the same tailnet as ${app.same_as} here. The two work side by side; disconnect it to let Tailfuse carry that tailnet alone.`
      : `It is on ${who || "another tailnet"}. The two work side by side; disconnect it if you want Tailfuse to carry everything.`;
  }

  $("#cli-install").textContent = s.cli ? "Reinstall the command" : "Install the command";
  if (!cliBusy) $("#cli-text").textContent = s.cli ? `Installed at ${s.cli}. In a terminal: tailfuse status` : "For a terminal: tailfuse status, tailfuse ssh and more.";

  const count = s.profiles.length;
  $("#profile-hint").textContent = count === 0 ? "" : count <= 2 ? `${count} of 2 free tailnets used` : `${count} tailnets, licensed`;
  $("#profiles-empty").hidden = count > 0;
  $("#profiles").replaceChildren(...s.profiles.map((p) => profileEntry(p, s.up)));
  $("#tunnels").replaceChildren(...s.tunnels.map(tunnelEntry));

  // The tailnet choice in the local-port form follows the tailnets that exist.
  const select = $("#t-profile");
  const chosen = select.value;
  select.replaceChildren(...s.profiles.map((p) => h("option", null, p.name)));
  if (s.profiles.some((p) => p.name === chosen)) select.value = chosen;
  $("#show-add-tunnel").disabled = count === 0;
}

async function refresh() {
  try {
    render(await invoke("get_state"));
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

    if (target.id === "proxy-fix" || target.id === "proxy-remove") {
      target.disabled = true;
      await call(target.id === "proxy-fix" ? "connect_browsers" : "disconnect_browsers").catch(() => {});
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
    button.disabled = true;
    button.textContent = "Waiting for your browser";
    msg.textContent = "Finish signing in in the browser page that just opened. This window updates when you're done.";
    try {
      const name = await invoke("sign_in_new_tailnet");
      msg.textContent = `${name} is signed in and coming online.`;
    } catch (err) {
      msg.textContent = sentence(err);
    }
    button.disabled = false;
    button.textContent = "Sign in with Tailscale";
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
        : `${host} is on none of your tailnets. Tailfuse refuses it, and your browser reaches it directly.`;
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
