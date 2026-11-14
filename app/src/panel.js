// The menu-bar panel. No bundler: the app exposes Tauri on window.__TAURI__
// (withGlobalTauri in tauri.conf.json); the preview stub covers design checks
// in a plain browser.
const tauri = window.__TAURI__?.core;
const isPreview = !tauri && location.protocol === "http:" && location.port !== "";
const invoke = tauri ? tauri.invoke : isPreview ? (await import("./preview-stub.js")).invoke : null;

const $ = (s) => document.querySelector(s);

async function refresh() {
  if (!invoke) return;
  let s;
  try {
    s = await invoke("get_state");
  } catch {
    return;
  }
  $("#power").checked = s.up;
  const online = s.profiles.filter((p) => p.online).length;
  const state = $("#state");
  if (!s.up) {
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
    dot.className = `dot ${p.online ? "on" : ""}`;
    const main = document.createElement("div");
    main.className = "row-main";
    const name = document.createElement("strong");
    name.textContent = p.name;
    const sub = document.createElement("span");
    sub.className = "mono";
    sub.textContent = p.online ? (p.account || p.suffixes.join("  ")) : p.paused ? "Disconnected" : s.up ? "Sign-in needed" : "Off";
    main.append(name, sub);
    li.append(dot, main);
    if (s.up && !p.online && !p.paused) {
      const b = document.createElement("button");
      b.className = "btn btn--primary btn--sm";
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

$("#power").addEventListener("change", async (e) => {
  if (!invoke) return;
  e.target.disabled = true;
  try {
    await invoke(e.target.checked ? "start" : "stop");
  } catch (err) {
    invoke("notify_user", { title: "Tailfuse could not turn on", body: String(err) }).catch(() => {});
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
