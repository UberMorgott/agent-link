"use strict";

const form = document.getElementById("form");
const result = document.getElementById("settings_result");
const code = document.getElementById("code");
const workDir = document.getElementById("work_dir");
const pick = document.getElementById("pick");
const agentPath = document.getElementById("agent_path");
const pickAgent = document.getElementById("pick_agent");
const findAgent = document.getElementById("find_agent");

// Letters and digits without 0/O and 1/I: 32 symbols, so a byte & 31 is uniform.
const CODE_ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";

function showSettings(s) {
  for (const key of ["node", "code", "work_dir", "listen", "api"]) {
    form.elements[key].value = s[key] || "";
  }
  form.elements.peer_addr.value = "";
  form.elements.areas.value = (s.areas || []).join(", ");
  form.elements.handler.value = s.handler || "none";
  agentPath.value = s.agent_path || "";
  showAgent();
  form.elements.autostart.checked = !!s.autostart;
  form.elements.discovery.checked = s.discovery !== false;
  form.elements.max_jobs.value = s.max_jobs ? String(s.max_jobs) : "";
  if (s.listen || s.api || s.discovery === false || s.max_jobs || (s.areas || []).length) document.getElementById("advanced").open = true;
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

// showMyAddr tells this side's address, the one the others type in, and
// lists the members of the network.
function showMyAddr(st) {
  const el = document.getElementById("my_addr");
  showMembers(st);
  if (!st.configured) { el.textContent = ""; return; }
  const addr = (st.listen || "").replace(/:7420$/, "");
  el.textContent = addr ? fmt("settings.my_addr", { addr }) : "";
  if (!st.zerotier) el.textContent += " " + t("settings.my_addr.none");
}

// showMembers renders the member table: this node first, then the others.
function showMembers(st) {
  const list = document.getElementById("members");
  const others = (st.members || []).filter((m) => !m.self);
  document.getElementById("members_none").hidden = !st.configured || others.length > 0;
  list.replaceChildren();
  for (const m of st.members || []) {
    const li = document.createElement("li");
    const who = document.createElement("span");
    who.className = "who";
    const name = document.createElement("strong");
    name.textContent = m.self ? fmt("settings.members.self", { name: m.name }) : m.name;
    who.append(name, " ");
    if (!m.self) {
      const state = document.createElement("span");
      state.className = m.online ? "on" : "off";
      state.textContent = t(m.online ? "settings.members.online" : "settings.members.lost");
      who.append(state);
    }
    const details = [];
    if (m.app) details.push(fmt("settings.members.version", { version: m.app }));
    if (m.old_auth) details.push(t("settings.members.old_auth"));
    else if (m.legacy) details.push(t("settings.members.legacy"));
    if (!m.online && m.seen) details.push(fmt("settings.members.seen", { when: new Date(m.seen).toLocaleString("ru-RU") }));
    if ((m.addrs || []).length) details.push(m.addrs.join(", "));
    if (details.length) {
      const d = document.createElement("span");
      d.className = "detail";
      d.textContent = details.join(" · ");
      who.append(d);
    }
    li.append(who);
    if (!m.self) {
      const rm = document.createElement("button");
      rm.type = "button";
      rm.textContent = t("settings.members.remove");
      rm.addEventListener("click", () => removeMember(m.name));
      li.append(rm);
    }
    list.append(li);
  }
}

async function removeMember(name) {
  if (!confirm(fmt("settings.members.confirm", { name }))) return;
  try {
    showMembers(await api("POST", "members/remove", { name }));
    result.textContent = fmt("settings.members.removed", { name });
  } catch (e) {
    result.textContent = e.message;
  }
}

// «Добавить» keeps the address and dials it now, without «Сохранить».
document.getElementById("add_peer").addEventListener("click", async () => {
  const addr = form.elements.peer_addr.value.trim();
  if (!addr) { result.textContent = t("settings.peer_addr.empty"); return; }
  try {
    showMembers(await api("POST", "members/add", { addr }));
    form.elements.peer_addr.value = "";
    result.textContent = fmt("settings.peer_addr.added", { addr });
  } catch (e) {
    result.textContent = e.message;
  }
});

// 12 symbols, 60 bits, shown as XXXX-XXXX-XXXX.
document.getElementById("generate").addEventListener("click", () => {
  const bytes = new Uint8Array(12);
  crypto.getRandomValues(bytes);
  const s = Array.from(bytes, (b) => CODE_ALPHABET[b & 31]).join("");
  code.value = s.slice(0, 4) + "-" + s.slice(4, 8) + "-" + s.slice(8);
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
    discovery: f.discovery.checked,
    // Empty is the default; anything that is not a whole number is sent as -1
    // so the server names the field instead of silently using the default.
    max_jobs: f.max_jobs.value.trim() === "" ? 0 : (/^\d+$/.test(f.max_jobs.value.trim()) ? Number(f.max_jobs.value.trim()) : -1),
    autostart: f.autostart.checked,
  };
  result.textContent = t("settings.saving");
  try {
    const r = await api("POST", "settings", body);
    const text = r.error ? r.error : t("settings.saved");
    result.textContent = r.found ? text + " " + r.found : text;
    await Promise.all([refreshSlice("settings"), refreshSlice("status")]);
  } catch (e) {
    result.textContent = e.message;
  }
});

// --- updates: own buttons and switch, not part of «Сохранить» ---

const updText = document.getElementById("update_text");
const updCheck = document.getElementById("update_check");
const updApply = document.getElementById("update_apply");
const updAuto = document.getElementById("update_auto");
let latest = "";

function showUpdate(u) {
  document.getElementById("update_version").textContent = fmt("update.version", { version: u.current });
  updText.textContent = u.text || "";
  updText.className = u.failed ? "hint failed" : "hint";
  updCheck.disabled = !u.enabled || u.busy;
  updApply.hidden = !u.available;
  updApply.disabled = u.busy;
  latest = u.latest || "";
  if (u.available) updApply.textContent = fmt("update.apply", { version: u.latest });
  updAuto.checked = !!u.auto;
  updAuto.disabled = !u.enabled;
}

async function updateAction(path, body, busyText) {
  if (busyText) updText.textContent = busyText;
  updCheck.disabled = updApply.disabled = true;
  try {
    showUpdate(await api("POST", path, body));
  } catch (e) {
    updText.textContent = e.message;
    updCheck.disabled = updApply.disabled = false;
  }
}

updCheck.addEventListener("click", () => updateAction("update/check", undefined, t("update.checking")));
updApply.addEventListener("click", () => updateAction("update/apply", undefined, fmt("update.applying", { version: latest })));
updAuto.addEventListener("change", () => updateAction("update/auto", { auto: updAuto.checked }));

store.subscribe("settings", showSettings);
store.subscribe("status", showMyAddr);
store.subscribe("update", showUpdate);
if (store.get().settings) showSettings(store.get().settings);
if (store.get().status) showMyAddr(store.get().status);
if (store.get().update) showUpdate(store.get().update);
