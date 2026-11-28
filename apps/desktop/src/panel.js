// The menu-bar panel. No bundler: the app exposes Tauri on window.__TAURI__
// (withGlobalTauri in tauri.conf.json); the preview stub covers design checks
// in a plain browser.
const tauri = window.__TAURI__?.core;
const isPreview = !tauri && location.protocol === "http:" && location.port !== "";
const invoke = tauri ? tauri.invoke : isPreview ? (await import("./preview-stub.js")).invoke : null;

const $ = (s) => document.querySelector(s);

/** True while the power switch's start or stop runs; refresh leaves the switch and the pill alone. */
let powerBusy = false;

async function refresh() {
  if (!invoke) return;
  let s;
  try {
    s = await invoke("get_state");
  } catch {
    return;
  }
  // Mid start, the tailnets coming up show as connecting (see main.js `render`).
  if (powerBusy && $("#power").checked && !s.up) {
    s = { ...s, up: true, profiles: s.profiles.map((p) => (p.paused || p.online ? p : { ...p, connecting: true })) };
  }
  // Mid start or stop the switch keeps where it was moved (see main.js `powerBusy`).
  if (!powerBusy) $("#power").checked = s.up;
  const online = s.profiles.filter((p) => p.online).length;
  const state = $("#state");
  if (powerBusy) {
    // "Starting…" / "Stopping…", set by the switch, stays until its command answers.
  } else if (!s.up) {
    state.textContent = "Stopped";
    state.className = "pill";
  } else if (online > 0) {
    state.textContent = `${online} connected`;
    state.className = "pill on";
  } else {
    state.textContent = "Waiting for sign-in";
    state.className = "pill wait";
  }

  $("#empty").hidden = s.profiles.length > 0;
  const ul = $("#profiles");
  ul.replaceChildren(...s.profiles.map((p) => {
    const li = document.createElement("li");
    li.className = "row";
    const dot = document.createElement("span");
    dot.className = `dot ${p.online ? "on" : p.connecting && s.up ? "connecting" : ""}`;
    const main = document.createElement("div");
    main.className = "row-main";
    // The account a tailnet signed in with is what people recognise it by;
    // the profile name moves to the second line with its state.
    const name = document.createElement("strong");
    name.textContent = p.account || p.name;
    name.title = p.account || p.name;
    const connecting = s.up && p.connecting && !p.online;
    const needsSignIn = s.up && !p.online && !p.paused && !p.connecting && !p.error;
    const status = p.online ? (p.ips?.[0] || "Connected") : p.error ? "Problem" : connecting ? "Connecting…" : p.paused ? "Off" : needsSignIn ? "Not signed in" : "Off";
    const sub = document.createElement("span");
    sub.className = "row-sub";
    const label = document.createElement("span");
    // The state first (what you act on); the tailnet's own name after it, cut first when short of room.
    label.className = "row-domain";
    label.textContent = p.account ? p.name : "";
    const stateEl = document.createElement("span");
    stateEl.className = `row-state${p.online ? " on mono" : ""}${needsSignIn || p.error ? " wait" : ""}`;
    stateEl.textContent = status;
    sub.append(...[stateEl, label.textContent && label].filter(Boolean));
    main.append(name, sub);
    li.append(dot, main);
    if (needsSignIn) {
      const b = document.createElement("button");
      b.className = "row-action";
      b.dataset.login = p.name;
      b.textContent = "Sign in";
      li.append(b);
    } else {
      const sw = document.createElement("label");
      sw.className = "switch switch--sm";
      sw.title = p.online ? `Disconnect ${p.name}` : `Connect ${p.name}`;
      const input = document.createElement("input");
      input.type = "checkbox";
      input.checked = Boolean(p.online);
      input.dataset.tailnet = p.name;
      input.setAttribute("aria-label", `${p.name} connected`);
      const slider = document.createElement("span");
      slider.className = "slider";
      sw.append(input, slider);
      li.append(sw);
    }
    return li;
  }));
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

/** Says why the last start or stop failed, in the panel itself (a notification may be switched off). */
function showProblem(text) {
  const el = $("#problem");
  el.textContent = text || "";
  el.hidden = !text;
}

$("#power").addEventListener("change", async (e) => {
  if (!invoke) return;
  const starting = e.target.checked;
  e.target.disabled = true;
  powerBusy = true;
  showProblem("");
  const state = $("#state");
  state.textContent = starting ? "Starting…" : "Stopping…";
  state.className = "pill wait busy";
  $(".p-head .switch").classList.add("busy");
  // Show the tailnets coming up at once, before the first refresh answers.
  refresh();
  try {
    await invoke(starting ? "start" : "stop");
  } catch (err) {
    const title = starting ? "Tailfuse could not turn on" : "Tailfuse could not turn off";
    showProblem(`${title}: ${String(err)}`);
    invoke("notify_user", { title, body: String(err) }).catch(() => {});
  } finally {
    powerBusy = false;
    $(".p-head .switch").classList.remove("busy");
  }
  e.target.disabled = false;
  refresh();
});

// Quick connect/disconnect for one tailnet.
document.addEventListener("change", async (e) => {
  const name = e.target.dataset?.tailnet;
  if (!name || !invoke) return;
  e.target.disabled = true;
  try {
    await invoke("set_tailnet", { name, on: e.target.checked });
  } catch (err) {
    invoke("notify_user", { title: "Tailfuse", body: String(err) }).catch(() => {});
  }
  e.target.disabled = false;
  refresh();
});

document.addEventListener("click", async (e) => {
  if (!invoke) return;
  const login = e.target.dataset?.login;
  if (login) await invoke("login", { name: login });
  refresh();
});

$("#open").addEventListener("click", () => invoke?.("open_main_window"));
$("#quit").addEventListener("click", () => invoke?.("quit_app"));

// Esc closes the panel (the window also hides on blur).
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") window.close();
});

refresh();
setInterval(refresh, 2000);

// Fit the window to the panel, so no empty strip hangs below it (fit_panel in lib.rs).
// The panel's own height is capped by the window (max-height: 100vh), so measure
// its content instead: each visible section's full height (a scrolling list
// reports all of its rows in scrollHeight), plus the panel's border.
let lastFit = 0;
function fitWindow() {
  const el = document.querySelector(".panel");
  if (!invoke || !el) return;
  let h = 0;
  for (const child of el.children) {
    if (child.hidden || getComputedStyle(child).display === "none") continue;
    const cs = getComputedStyle(child);
    h += child.scrollHeight + parseFloat(cs.marginTop) + parseFloat(cs.marginBottom);
  }
  const ps = getComputedStyle(el);
  h = Math.ceil(h + parseFloat(ps.borderTopWidth) + parseFloat(ps.borderBottomWidth));
  if (h && h !== lastFit) {
    lastFit = h;
    invoke("fit_panel", { height: h }).catch(() => {});
  }
}
if (window.ResizeObserver) new ResizeObserver(fitWindow).observe(document.querySelector(".panel"));
setInterval(fitWindow, 1000);
fitWindow();
