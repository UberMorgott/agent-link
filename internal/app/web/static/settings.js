"use strict";

const form = document.getElementById("form");
const result = document.getElementById("result");
const code = document.getElementById("code");

// Letters and digits without 0/O and 1/I: 32 symbols, so a byte & 31 is uniform.
const CODE_ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";

async function load() {
  const s = await api("GET", "settings");
  for (const key of ["node", "code", "peer_addr", "work_dir", "listen", "api", "peer_name"]) {
    form.elements[key].value = s[key] || "";
  }
  form.elements.areas.value = (s.areas || []).join(", ");
  form.elements.handler.value = s.handler || "none";
  form.elements.autostart.checked = !!s.autostart;
  if (s.listen || s.api || s.peer_name || (s.areas || []).length) document.getElementById("advanced").open = true;
}

// showMyAddr tells this side's address, the one the other person types in.
async function showMyAddr() {
  const el = document.getElementById("my_addr");
  try {
    const st = await api("GET", "status");
    if (!st.configured) { el.textContent = ""; return; }
    if (!st.zerotier) { el.textContent = t("settings.my_addr.none"); return; }
    const addr = (st.listen || "").replace(/:7420$/, "");
    el.textContent = addr ? fmt("settings.my_addr", { addr }) : "";
  } catch (_) { el.textContent = ""; }
}

document.getElementById("generate").addEventListener("click", () => {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  code.value = Array.from(bytes, (b) => CODE_ALPHABET[b & 31]).join("");
  result.textContent = t("settings.code.generated");
});

document.getElementById("copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(code.value.toUpperCase());
    result.textContent = t("settings.code.copied");
  } catch (_) {
    code.select();
    result.textContent = t("settings.code.copy_manual");
  }
});

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = form.elements;
  const body = {
    node: f.node.value.trim(),
    code: f.code.value.trim(),
    peer_addr: f.peer_addr.value.trim(),
    handler: f.handler.value,
    work_dir: f.work_dir.value.trim(),
    listen: f.listen.value.trim(),
    api: f.api.value.trim(),
    areas: f.areas.value.split(",").map((a) => a.trim()).filter(Boolean),
    peer_name: f.peer_name.value.trim(),
    autostart: f.autostart.checked,
  };
  result.textContent = t("settings.saving");
  try {
    const r = await api("POST", "settings", body);
    result.textContent = r.error ? r.error : t("settings.saved");
    await load();
    refreshStatus();
    showMyAddr();
  } catch (e) {
    result.textContent = e.message;
  }
});

load().catch((e) => { result.textContent = e.message; });
showMyAddr();
setInterval(showMyAddr, 5000);
