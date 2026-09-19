"use strict";

const TOKEN = document.querySelector('meta[name="agentlink-token"]').content;
const CORE_SLICES = new Set(["status", "dashboard", "participants", "update"]);
const inFlight = new Map();

function createStore(initial) {
  let state = Object.assign({}, initial);
  const listeners = new Map();
  return {
    get: () => state,
    patch(name, value) {
      if (Object.is(state[name], value)) return;
      state = Object.assign({}, state, { [name]: value });
      for (const fn of listeners.get(name) || []) fn(value, state);
    },
    subscribe(name, fn) {
      if (!listeners.has(name)) listeners.set(name, new Set());
      listeners.get(name).add(fn);
      return () => listeners.get(name).delete(fn);
    },
  };
}

const store = createStore({ status: null, dashboard: null, participants: null, update: null });

// STRINGS is the application-shell dictionary served by the app; see internal/app/strings.go.
const STRINGS = JSON.parse(document.querySelector('meta[name="agentlink-strings"]').content);

// t returns the text for a key; fmt also fills its {name} placeholders.
function t(key) {
  return Object.prototype.hasOwnProperty.call(STRINGS, key) ? STRINGS[key] : key;
}

function fmt(key, vars) {
  return t(key).replace(/\{(\w+)\}/g, (m, name) => (name in vars ? String(vars[name]) : m));
}

// applyStrings fills every [data-t] element and the shell's initial page title.
function applyStrings(root) {
  for (const el of (root || document).querySelectorAll("[data-t]")) el.textContent = t(el.dataset.t);
  const title = document.querySelector('meta[name="agentlink-title"]');
  if (title) document.title = t(title.content);
}

function api(method, path, body) {
  const key = method === "GET" && body === undefined && CORE_SLICES.has(path) ? path : "";
  if (key && inFlight.has(key)) return inFlight.get(key);
  const request = apiRequest(method, path, body);
  if (key) {
    inFlight.set(key, request);
    request.finally(() => {
      if (inFlight.get(key) === request) inFlight.delete(key);
    }).catch(() => {});
  }
  return request;
}

async function apiRequest(method, path, body) {
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
    const error = new Error(resp.status === 403 ? t("error.forbidden") : (typeof data === "object" && data && data.error ? data.error : t("error.internal")));
    error.status = resp.status;
    throw error;
  }
  return data;
}

function renderStatus(s) {
  const el = document.getElementById("status");
  let text, cls = "off";
  if (!s.configured) text = t("link.unconfigured");
  else if (s.error) text = s.error;
  else if (s.connected) {
    text = fmt("link.on_many", { online: s.online || 0, total: s.total || 0 });
    cls = "on";
  }
  else if (s.problem) text = t(s.problem);
  else text = fmt("link.off_count", { online: s.online || 0, total: s.total || 0 });
  if (s.configured && !s.zerotier) text += " · " + t("link.no_zerotier");
  if (s.warning) { text += " · " + t(s.warning); cls = "off"; }
  el.textContent = text;
  el.className = cls;
  const names = document.getElementById("member_names");
  if (names) {
    names.replaceChildren(...(s.members || []).filter((m) => !m.self).map((m) => {
      const o = document.createElement("option");
      o.value = m.name;
      return o;
    }));
  }
}

let reloadRequired = false;

function showConnectionProblem(error) {
  const region = document.getElementById("toast-region");
  if (!region) return;
  if (error.status === 403) {
    reloadRequired = true;
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = t("connection.reload");
    button.addEventListener("click", () => location.reload());
    region.replaceChildren(document.createTextNode(error.message + " "), button);
    return;
  }
  if (!reloadRequired) region.textContent = error.message || t("link.app_not_running");
}

function clearConnectionProblem() {
  if (!reloadRequired) document.getElementById("toast-region")?.replaceChildren();
}

function refreshSlice(name) {
  return api("GET", name).then((value) => {
    store.patch(name, value);
    clearConnectionProblem();
  }).catch((error) => showConnectionProblem(error));
}

function refreshCore() {
  return Promise.all([...CORE_SLICES].map(refreshSlice));
}

applyStrings();
store.subscribe("status", renderStatus);
function refreshStatus() { return refreshCore(); }
refreshCore();
setInterval(refreshCore, 3000);
