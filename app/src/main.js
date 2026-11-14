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

// One place for things that went wrong, in the window instead of a system alert.
let errorTimer = 0;
function showError(err) {
  const el = $("#error");
  el.textContent = sentence(err);
  el.hidden = false;
  clearTimeout(errorTimer);
  errorTimer = setTimeout(() => (el.hidden = true), 9000);
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
  return `${plural(online, "tailnet")} online. Open any of their addresses in your browser as usual.`;
}

let lastState = null;
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
  $("#pac-url").textContent = s.pac_url;

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
    if (target.id === "show-add-profile") return toggleForm("add-profile", true);
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

  $("#add-profile").addEventListener("submit", async (e) => {
    e.preventDefault();
    let suffix = $("#p-suffix").value.trim();
    if (suffix && !suffix.startsWith(".")) suffix = `.${suffix}`;
    try {
      await call("add_profile", { name: $("#p-name").value.trim(), suffixes: [suffix] });
      e.target.reset();
      toggleForm("add-profile", false);
    } catch {
      // the error line already says why
    }
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
