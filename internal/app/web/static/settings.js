"use strict";

const form = document.getElementById("form");
const result = document.getElementById("result");
const secret = document.getElementById("secret");

async function load() {
  const s = await api("GET", "settings");
  for (const key of ["node", "listen", "peer_name", "peer_addr", "secret", "work_dir"]) {
    form.elements[key].value = s[key] || "";
  }
  form.elements.areas.value = (s.areas || []).join(", ");
  form.elements.handler.value = s.handler || "none";
  form.elements.autostart.checked = !!s.autostart;
}

document.getElementById("generate").addEventListener("click", () => {
  const bytes = new Uint8Array(48);
  crypto.getRandomValues(bytes);
  secret.value = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  secret.type = "text";
  result.textContent = "New secret generated. Copy it and send it to the other person privately, then Save.";
});

document.getElementById("show").addEventListener("click", () => {
  secret.type = secret.type === "password" ? "text" : "password";
});

document.getElementById("copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(secret.value);
    result.textContent = "Secret copied.";
  } catch (_) {
    secret.type = "text";
    secret.select();
    result.textContent = "Press Ctrl+C to copy the selected secret.";
  }
});

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = form.elements;
  const body = {
    node: f.node.value.trim(),
    listen: f.listen.value.trim(),
    peer_name: f.peer_name.value.trim(),
    peer_addr: f.peer_addr.value.trim(),
    secret: f.secret.value,
    areas: f.areas.value.split(",").map((a) => a.trim()).filter(Boolean),
    handler: f.handler.value,
    work_dir: f.work_dir.value.trim(),
    autostart: f.autostart.checked,
  };
  result.textContent = "Saving...";
  try {
    const r = await api("POST", "settings", body);
    result.textContent = r.error ? r.error : "Saved. agentlink restarted with the new settings.";
    refreshStatus();
  } catch (e) {
    result.textContent = "Not saved: " + e.message;
  }
});

load().catch((e) => { result.textContent = "Could not load settings: " + e.message; });
