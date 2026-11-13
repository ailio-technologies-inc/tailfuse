import { invoke } from "@tauri-apps/api/core";

const $ = (sel) => document.querySelector(sel);

async function refresh() {
  let s;
  try {
    s = await invoke("get_state");
  } catch (e) {
    console.error(e);
    return;
  }

  $("#license").textContent = s.license;
  $("#pac-url").textContent = s.pac_url;
  $("#power").textContent = s.up ? "Stop" : "Start";
  $("#power").classList.toggle("on", s.up);

  const ul = $("#profiles");
  ul.innerHTML = "";
  for (const p of s.profiles) {
    const li = document.createElement("li");
    li.className = "card";
    const state = p.error ? `error: ${p.error}` : p.online ? "online" : "logged out";
    li.innerHTML = `
      <span class="dot ${p.online ? "on" : ""}"></span>
      <div class="card-body">
        <strong>${escapeHtml(p.name)}</strong>
        <span class="muted">${p.suffixes.map(escapeHtml).join(", ")}</span>
        <span class="muted">${escapeHtml(p.hostname)} ${p.ips.join(", ")}</span>
        <span class="state">${escapeHtml(state)}</span>
      </div>
      <div class="card-actions">
        ${p.online ? "" : `<button data-login="${escapeHtml(p.name)}">Log in</button>`}
        <button class="danger" data-remove-profile="${escapeHtml(p.name)}">Remove</button>
      </div>`;
    ul.appendChild(li);
  }

  const tl = $("#tunnels");
  tl.innerHTML = "";
  for (const t of s.tunnels) {
    const li = document.createElement("li");
    li.className = "card";
    li.innerHTML = `
      <span class="dot tunnel"></span>
      <div class="card-body">
        <strong>${escapeHtml(t.name)}</strong>
        <span class="muted">${escapeHtml(t.listen)} → ${escapeHtml(t.target)}</span>
        <span class="muted">via ${escapeHtml(t.profile)}</span>
      </div>
      <div class="card-actions">
        <button class="danger" data-remove-tunnel="${escapeHtml(t.name)}">Remove</button>
      </div>`;
    tl.appendChild(li);
  }
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

$("#power").addEventListener("click", async () => {
  const on = $("#power").classList.contains("on");
  await invoke(on ? "stop" : "start");
  refresh();
});

document.addEventListener("click", async (e) => {
  const login = e.target.dataset?.login;
  const rmP = e.target.dataset?.removeProfile;
  const rmT = e.target.dataset?.removeTunnel;
  if (login) await invoke("login", { name: login });
  if (rmP && confirm(`Remove profile ${rmP}?`)) await invoke("remove_profile", { name: rmP });
  if (rmT) await invoke("remove_tunnel", { name: rmT });
  refresh();
});

$("#add-profile").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await invoke("add_profile", {
      name: $("#p-name").value.trim(),
      suffixes: [$("#p-suffix").value.trim()],
    });
    $("#p-name").value = $("#p-suffix").value = "";
  } catch (err) {
    alert(String(err));
  }
  refresh();
});

$("#add-tunnel").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await invoke("add_tunnel", {
      name: $("#t-name").value.trim(),
      listen: $("#t-listen").value.trim(),
      target: $("#t-target").value.trim(),
      profile: $("#t-profile").value.trim(),
    });
    e.target.reset();
  } catch (err) {
    alert(String(err));
  }
  refresh();
});

$("#activate").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const msg = await invoke("activate_license", { key: $("#license-key").value.trim() });
    $("#license-msg").textContent = msg;
    $("#license-key").value = "";
  } catch (err) {
    $("#license-msg").textContent = String(err);
  }
  refresh();
});

$("#test-host").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#test-result").textContent = await invoke("test_host", {
    host: $("#test-input").value.trim(),
  });
});

refresh();
setInterval(refresh, 3000);
