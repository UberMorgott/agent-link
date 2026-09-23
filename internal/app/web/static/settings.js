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
const DEFAULT_API = "127.0.0.1:7520";

// Every change saves itself: toggles, pickers and row removal at once, text
// fields on "change" (blur or Enter), so a half-typed value never restarts the
// node. settingsEdits counts edits in the form; the form is repainted from the
// server only when every edit has been saved.
let settingsEdits = 0;
let settingsSettled = 0;
let settingsSaving = false;
let settingsSaveAgain = false;
let settingsLastSent = "";

// --- «Проекты»: an area mapped to a project folder ---

const projectList = document.getElementById("projects");
let projectRows = [];

function effectiveAPI(settings) {
  return String(settings?.api || DEFAULT_API).trim();
}

function labelled(text, control) {
  const label = document.createElement("label");
  const span = document.createElement("span");
  span.textContent = text;
  label.append(span, control);
  return label;
}

// projectRow builds one editable project card: area and folder.
function projectRow(area, project) {
  const li = document.createElement("li");
  li.className = "project-card";
  const areaInput = document.createElement("input");
  areaInput.value = area;
  areaInput.autocomplete = "off";
  areaInput.spellcheck = false;
  const dirInput = document.createElement("input");
  dirInput.value = project.dir || "";
  dirInput.autocomplete = "off";
  dirInput.spellcheck = false;
  const pickDir = document.createElement("button");
  pickDir.type = "button";
  pickDir.textContent = t("settings.work_dir.pick");
  pickDir.addEventListener("click", async () => {
    if (await pickFolder(pickDir, dirInput)) editedAndSave();
  });
  const dirRow = document.createElement("span");
  dirRow.className = "row";
  dirRow.append(dirInput, pickDir);
  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "project-remove";
  remove.textContent = t("settings.projects.remove");
  const hooks = document.createElement("p");
  hooks.className = "hint";
  li.append(labelled(t("settings.projects.area"), areaInput), labelled(t("settings.projects.dir"), dirRow), hooks, remove);
  const row = { li, area: areaInput, dir: dirInput, hooks };
  remove.addEventListener("click", () => {
    projectRows = projectRows.filter((r) => r !== row);
    showProjects();
    editedAndSave();
  });
  return row;
}

function showProjects() {
  projectList.replaceChildren(...projectRows.map((r) => r.li));
  document.getElementById("projects_empty").hidden = projectRows.length > 0;
}

document.getElementById("add_project").addEventListener("click", () => {
  const row = projectRow("", {});
  projectRows.push(row);
  showProjects();
  row.area.focus();
});

// projectsBody reads the rows: a blank row is skipped, a row with only an area
// or only a folder makes the rows incomplete, and one area twice is a duplicate.
function projectsBody() {
  const out = {};
  let complete = true, duplicate = false;
  for (const r of projectRows) {
    const area = r.area.value.trim(), dir = r.dir.value.trim();
    if (!area && !dir) continue;
    if (!area || !dir) { complete = false; continue; }
    if (Object.hasOwn(out, area)) { duplicate = true; continue; }
    out[area] = { dir };
  }
  return { projects: out, valid: complete && !duplicate, duplicate };
}

// projectsKey compares project sets regardless of row order.
function projectsKey(projects) {
  return JSON.stringify(Object.keys(projects || {}).sort().map((area) => [area, projects[area].dir || ""]));
}

// settingsBody is the save request. Rows that are not valid yet are not sent:
// the saved projects go instead, so a half-typed row never replaces them.
function settingsBody(rows) {
  const f = form.elements;
  return {
    node: f.node.value.trim(),
    code: f.code.value.trim(),
    handler: f.handler.value,
    agent_path: agentPath.value,
    work_dir: f.work_dir.value.trim(),
    listen: f.listen.value.trim(),
    api: f.api.value.trim(),
    areas: f.areas.value.split(",").map((a) => a.trim()).filter(Boolean),
    projects: rows.valid ? rows.projects : (store.get().settings?.projects || {}),
    discovery: f.discovery.checked,
    // Empty is the default; anything that is not a whole number is sent as -1
    // so the server names the field instead of silently using the default.
    max_jobs: f.max_jobs.value.trim() === "" ? 0 : (/^\d+$/.test(f.max_jobs.value.trim()) ? Number(f.max_jobs.value.trim()) : -1),
    autostart: f.autostart.checked,
  };
}

function showSettings(s) {
  // Unsaved edits win over a repaint; the next save brings them together.
  if (settingsSaving || settingsEdits !== settingsSettled) return;
  for (const key of ["node", "code", "work_dir", "listen", "api"]) {
    const value = s[key] || "";
    if (form.elements[key].value !== value) form.elements[key].value = value;
  }
  form.elements.areas.value = (s.areas || []).join(", ");
  form.elements.handler.value = s.handler || "none";
  agentPath.value = s.agent_path || "";
  showAgent();
  form.elements.autostart.checked = !!s.autostart;
  form.elements.discovery.checked = s.discovery !== false;
  form.elements.max_jobs.value = s.max_jobs ? String(s.max_jobs) : "";
  if (s.listen || s.api || s.discovery === false || s.max_jobs || (s.areas || []).length) document.getElementById("advanced").open = true;
  showWorkDir("settings.work_dir.current");
  const projects = s.projects || {};
  // Rebuilding equal rows would only take the focus away from them.
  const rows = projectsBody();
  if (!rows.valid || projectsKey(rows.projects) !== projectsKey(projects)) {
    projectRows = Object.keys(projects).sort().map((area) => projectRow(area, projects[area]));
    showProjects();
  }
  settingsLastSent = JSON.stringify(settingsBody(projectsBody()));
}

// saveSettings sends the form when it differs from what was last sent. One
// request runs at a time; a change during it saves again afterwards. note
// replaces «Сохранено.» on success.
async function saveSettings(note) {
  if (settingsSaving) { settingsSaveAgain = true; return; }
  const rows = projectsBody();
  const body = settingsBody(rows);
  const sent = JSON.stringify(body), edits = settingsEdits;
  if (rows.duplicate) result.textContent = t("error.projects_twice");
  if (sent === settingsLastSent) {
    if (rows.valid) settingsSettled = edits;
    return;
  }
  const previousAPI = effectiveAPI(store.get().settings);
  if (!rows.duplicate) result.textContent = t("settings.saving");
  settingsSaving = true;
  form.setAttribute("aria-busy", "true");
  try {
    const r = await api("POST", "settings", body);
    settingsLastSent = sent;
    const text = r.error ? r.error : (note || t("settings.saved"));
    if (!rows.duplicate) result.textContent = r.found ? text + " " + r.found : text;
    settingsSaving = false;
    if (rows.valid && settingsEdits === edits) settingsSettled = edits;
    if (r.settings) store.patch("settings", r.settings);
    if (r.status) store.patch("status", r.status);
    if (r.dashboard) store.patch("dashboard", r.dashboard);
    if (r.settings && effectiveAPI(r.settings) !== previousAPI) location.reload();
  } catch (e) {
    result.textContent = e.message;
  } finally {
    settingsSaving = false;
    form.removeAttribute("aria-busy");
  }
  if (settingsSaveAgain) {
    settingsSaveAgain = false;
    await saveSettings();
  }
}

// editedAndSave saves a value that a script put into the form: such a value
// fires neither "input" nor "change".
function editedAndSave(note) {
  settingsEdits++;
  return saveSettings(note);
}

// The auto-update switch sits in the form but saves through its own request.
const ownRequest = (ev) => ev.target?.id === "update_auto";
form.addEventListener("input", (ev) => { if (!ownRequest(ev)) settingsEdits++; });
form.addEventListener("change", (ev) => { if (!ownRequest(ev)) saveSettings(); });
form.addEventListener("submit", (ev) => {
  ev.preventDefault();
  saveSettings();
});

// showWorkDir repeats the chosen folder in full under the field.
function showWorkDir(key) {
  const el = document.getElementById("work_dir_shown");
  const path = workDir.value.trim();
  el.textContent = path ? fmt(key, { path }) : t("settings.work_dir.empty");
}

workDir.addEventListener("input", () => showWorkDir("settings.work_dir.current"));

// The tray process opens the native Windows folder dialog: a page cannot see
// absolute paths on disk. pickFolder puts the chosen folder into input and
// returns true.
async function pickFolder(button, input) {
  button.disabled = true;
  result.textContent = t("settings.work_dir.picking");
  try {
    const r = await api("POST", "pick-folder", { start: input.value.trim() });
    if (r.path) {
      input.value = r.path;
      result.textContent = "";
      return true;
    }
    result.textContent = r.message || "";
  } catch (e) {
    result.textContent = e.message;
  } finally {
    button.disabled = false;
  }
  return false;
}

pick.addEventListener("click", async () => {
  if (!await pickFolder(pick, workDir)) return;
  showWorkDir("settings.work_dir.chosen");
  await editedAndSave();
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

// A program chosen for one agent is not the other agent's program. The form's
// own "change" listener then saves the new handler.
form.elements.handler.addEventListener("change", () => {
  agentPath.value = "";
  showAgent();
});

// «Найти заново» looks through every known install location and saves the
// program it finds, like a picked program.
findAgent.addEventListener("click", async () => {
  findAgent.disabled = true;
  result.textContent = t("settings.agent.finding");
  const handler = form.elements.handler.value;
  try {
    const r = await api("POST", "find-agent", { handler });
    if (form.elements.handler.value !== handler) return;
    document.getElementById("agent_shown").textContent = r.text || "";
    result.textContent = "";
    if (r.source !== "missing") {
      agentPath.value = r.path || "";
      await editedAndSave();
    }
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
      await editedAndSave(fmt("settings.agent.chosen", { path: r.path }));
    } else {
      result.textContent = r.message || "";
    }
  } catch (e) {
    result.textContent = e.message;
  } finally {
    pickAgent.disabled = false;
  }
});

// showLocalAddress keeps this machine's advertised address with the advanced
// network configuration instead of presenting it as a participant.
function showLocalAddress(st) {
  const el = document.getElementById("my_addr");
  if (!st.configured) { el.textContent = ""; return; }
  const addr = (st.listen || "").replace(/:7420$/, "");
  el.textContent = addr ? fmt("settings.my_addr", { addr }) : "";
  if (!st.zerotier) el.textContent += " " + t("settings.my_addr.none");
}

// 12 symbols, 60 bits, shown as XXXX-XXXX-XXXX.
document.getElementById("generate").addEventListener("click", () => {
  const bytes = new Uint8Array(12);
  crypto.getRandomValues(bytes);
  const s = Array.from(bytes, (b) => CODE_ALPHABET[b & 31]).join("");
  code.value = s.slice(0, 4) + "-" + s.slice(4, 8) + "-" + s.slice(8);
  return editedAndSave(t("settings.code.generated"));
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

// --- hooks: the chosen agent's hook in the working and project folders ---

function hookText(client, state) {
  if (!client || !state) return "";
  if (state === "ok") return fmt("settings.hooks.ok", { agent: client === "codex" ? "Codex" : "Claude" });
  return t("settings.hooks." + state);
}

// showHooks reads what the app installed at the last save or start.
async function showHooks() {
  let h;
  try { h = await api("GET", "hooks"); } catch (_) { return; }
  const projects = h.projects || {};
  document.getElementById("work_dir_hooks").textContent = hookText(h.client, h.work_dir);
  for (const r of projectRows) r.hooks.textContent = hookText(h.client, projects[r.area.value.trim()]);
  document.getElementById("hooks_codex").hidden = !(h.client === "codex" && [h.work_dir, ...Object.values(projects)].includes("ok"));
}

// --- updates: own buttons and switch, saved by their own requests ---

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
    const update = await api("POST", path, body);
    store.patch("update", update);
  } catch (e) {
    updText.textContent = e.message;
    updCheck.disabled = updApply.disabled = false;
  }
}

updCheck.addEventListener("click", () => updateAction("update/check", undefined, t("update.checking")));
updApply.addEventListener("click", () => updateAction("update/apply", undefined, fmt("update.applying", { version: latest })));
updAuto.addEventListener("change", () => updateAction("update/auto", { auto: updAuto.checked }));

store.subscribe("settings", showSettings);
store.subscribe("status", showLocalAddress);
store.subscribe("update", showUpdate);
store.subscribe("settings", showHooks);
if (store.get().settings) { showSettings(store.get().settings); showHooks(); }
if (store.get().status) showLocalAddress(store.get().status);
if (store.get().update) showUpdate(store.get().update);
