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
  result.textContent = t("settings.secret.generated");
});

document.getElementById("show").addEventListener("click", () => {
  secret.type = secret.type === "password" ? "text" : "password";
});

document.getElementById("copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(secret.value);
    result.textContent = t("settings.secret.copied");
  } catch (_) {
    secret.type = "text";
    secret.select();
    result.textContent = t("settings.secret.copy_manual");
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
  result.textContent = t("settings.saving");
  try {
    const r = await api("POST", "settings", body);
    result.textContent = r.error ? r.error : t("settings.saved");
    refreshStatus();
  } catch (e) {
    result.textContent = fmt("settings.save_failed", { error: e.message });
  }
});

load().catch((e) => { result.textContent = fmt("settings.load_failed", { error: e.message }); });
