"use strict";

const form = document.getElementById("form");
const result = document.getElementById("result");
const code = document.getElementById("code");
const workDir = document.getElementById("work_dir");
const pick = document.getElementById("pick");
const agentPath = document.getElementById("agent_path");
const pickAgent = document.getElementById("pick_agent");
const findAgent = document.getElementById("find_agent");

// Letters and digits without 0/O and 1/I: 32 symbols, so a byte & 31 is uniform.
const CODE_ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";

async function load() {
  const s = await api("GET", "settings");
  for (const key of ["node", "code", "peer_addr", "work_dir", "listen", "api", "peer_name"]) {
    form.elements[key].value = s[key] || "";
  }
  form.elements.areas.value = (s.areas || []).join(", ");
  form.elements.handler.value = s.handler || "none";
  agentPath.value = s.agent_path || "";
  showAgent();
  form.elements.autostart.checked = !!s.autostart;
  if (s.listen || s.api || s.peer_name || (s.areas || []).length) document.getElementById("advanced").open = true;
  showWorkDir("settings.work_dir.current");
}

// showWorkDir repeats the chosen folder in full under the field.
function showWorkDir(key) {
  const el = document.getElementById("work_dir_shown");
  const path = workDir.value.trim();
  el.textContent = path ? fmt(key, { path }) : t("settings.work_dir.empty");
}

workDir.addEventListener("input", () => showWorkDir("settings.work_dir.current"));

// The tray process opens the native Windows folder dialog: a page cannot see
// absolute paths on disk.
pick.addEventListener("click", async () => {
  pick.disabled = true;
  result.textContent = t("settings.work_dir.picking");
  try {
    const r = await api("POST", "pick-folder", { start: workDir.value.trim() });
    if (r.path) {
      workDir.value = r.path;
      showWorkDir("settings.work_dir.chosen");
      result.textContent = "";
    } else {
      result.textContent = r.message || "";
    }
  } catch (e) {
    result.textContent = e.message;
  } finally {
    pick.disabled = false;
  }
});

// showAgent tells which agent program the chosen handler would run.
async function showAgent() {
  const row = document.getElementById("agent_row");
  const handler = form.elements.handler.value;
  row.hidden = handler === "none";
  if (row.hidden) return;
  const el = document.getElementById("agent_shown");
  try {
    const r = await api("POST", "agent", { handler, agent_path: agentPath.value });
    if (form.elements.handler.value === handler) el.textContent = r.text || "";
  } catch (e) { el.textContent = e.message; }
}

// A program chosen for one agent is not the other agent's program.
form.elements.handler.addEventListener("change", () => {
  agentPath.value = "";
  showAgent();
});

// «Найти заново» looks through every known install location; the result is
// kept until «Сохранить», like a picked program.
findAgent.addEventListener("click", async () => {
  findAgent.disabled = true;
  result.textContent = t("settings.agent.finding");
  const handler = form.elements.handler.value;
  try {
    const r = await api("POST", "find-agent", { handler });
    if (form.elements.handler.value !== handler) return;
    if (r.source !== "missing") agentPath.value = r.path || "";
    document.getElementById("agent_shown").textContent = r.text || "";
    result.textContent = r.source === "missing" ? "" : t("settings.agent.save_hint");
  } catch (e) {
    result.textContent = e.message;
  } finally {
    findAgent.disabled = false;
  }
});

// The tray process opens the native Windows file dialog for the program.
pickAgent.addEventListener("click", async () => {
  pickAgent.disabled = true;
  result.textContent = t("settings.agent.picking");
  try {
    const r = await api("POST", "pick-agent", { start: agentPath.value });
    if (r.path) {
      agentPath.value = r.path;
      await showAgent();
      result.textContent = fmt("settings.agent.chosen", { path: r.path });
    } else {
      result.textContent = r.message || "";
    }
  } catch (e) {
    result.textContent = e.message;
  } finally {
    pickAgent.disabled = false;
  }
});

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
    agent_path: agentPath.value,
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
    const text = r.error ? r.error : t("settings.saved");
    result.textContent = r.found ? text + " " + r.found : text;
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
