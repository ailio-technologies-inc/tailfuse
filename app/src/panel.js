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
  ul.innerHTML = "";
  for (const p of s.profiles) {
    const li = document.createElement("li");
    li.className = "row";
    li.innerHTML = `
      <span class="dot ${p.online ? "on" : ""}"></span>
      <div class="row-main">
        <strong>${escapeHtml(p.name)}</strong>
        <span class="mono">${escapeHtml(p.suffixes.join("  "))}</span>
      </div>
      ${p.online
        ? `<span class="pill on">on</span>`
        : s.up
          ? `<button class="btn btn--primary btn--sm" data-login="${escapeHtml(p.name)}">Sign in</button>`
          : `<span class="pill">off</span>`}
    `;
    ul.appendChild(li);
  }
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
    alert(String(err));
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
