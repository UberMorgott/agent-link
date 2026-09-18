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
  let resp;
  try {
    resp = await fetch("/ui/api/" + path, opts);
  } catch (_) {
    throw new Error(t("error.no_app"));
  }
  const text = await resp.text();
  let data = text;
  try { data = JSON.parse(text); } catch (_) { /* plain-text error */ }
  if (!resp.ok) {
    // The token changes on every start: a tab left open from before gets 403.
    if (resp.status === 403) throw new Error(t("error.forbidden"));
    throw new Error(typeof data === "object" && data && data.error ? data.error : t("error.internal"));
  }
  return data;
}

async function refreshStatus() {
  const el = document.getElementById("status");
  try {
    const s = await api("GET", "status");
    let text, cls = "off";
    if (!s.configured) text = t("link.unconfigured");
    else if (s.error) text = s.error;
    else if (s.connected) {
      text = s.total > 1 ? fmt("link.on_many", { online: s.online, total: s.total }) : fmt("link.on", { peer: s.peer });
      cls = "on";
    }
    else if (s.problem) text = t(s.problem);
    else text = s.peer ? fmt("link.off", { peer: s.peer }) : t("link.waiting");
    if (s.configured && !s.zerotier) text += " · " + t("link.no_zerotier");
    el.textContent = text;
    el.className = cls;
    // The inbox's «Кому» suggests every member by name.
    const names = document.getElementById("member_names");
    if (names) {
      names.replaceChildren(...(s.members || []).filter((m) => !m.self).map((m) => {
        const o = document.createElement("option");
        o.value = m.name;
        return o;
      }));
    }
  } catch (e) {
    el.textContent = t("link.app_not_running");
    el.className = "off";
  }
}

applyStrings();
refreshStatus();
setInterval(refreshStatus, 3000);
