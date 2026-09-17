"use strict";

const TOKEN = document.querySelector('meta[name="agentlink-token"]').content;

// STRINGS is the page dictionary served by the app; see internal/app/strings.go.
const STRINGS = JSON.parse(document.querySelector('meta[name="agentlink-strings"]').content);

// t returns the text for a key; fmt also fills its {name} placeholders.
function t(key) {
  return Object.prototype.hasOwnProperty.call(STRINGS, key) ? STRINGS[key] : key;
}

function fmt(key, vars) {
  return t(key).replace(/\{(\w+)\}/g, (m, name) => (name in vars ? String(vars[name]) : m));
}

// applyStrings fills every [data-t] element and the page title.
function applyStrings(root) {
  for (const el of (root || document).querySelectorAll("[data-t]")) el.textContent = t(el.dataset.t);
  const title = document.querySelector('meta[name="agentlink-title"]');
  if (title) document.title = t(title.content);
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Agentlink-Token": TOKEN } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch("/ui/api/" + path, opts);
  const text = await resp.text();
  let data = text;
  try { data = JSON.parse(text); } catch (_) { /* plain-text error */ }
  if (!resp.ok) {
    const msg = typeof data === "object" && data && data.error ? data.error : String(data).trim();
    throw new Error(msg || resp.statusText);
  }
  return data;
}

async function refreshStatus() {
  const el = document.getElementById("status");
  try {
    const s = await api("GET", "status");
    let text, cls;
    if (!s.configured) { text = t("link.unconfigured"); cls = "off"; }
    else if (s.error) { text = fmt("link.error", { error: s.error }); cls = "off"; }
    else if (s.connected) { text = fmt("link.on", { peer: s.peer }); cls = "on"; }
    else { text = fmt("link.off", { peer: s.peer }); cls = "off"; }
    el.textContent = text;
    el.className = cls;
  } catch (e) {
    el.textContent = t("link.app_not_running");
    el.className = "off";
  }
}

applyStrings();
refreshStatus();
setInterval(refreshStatus, 3000);
