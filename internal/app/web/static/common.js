"use strict";

const TOKEN = document.querySelector('meta[name="agentlink-token"]').content;
// PAGE_VERSION is the app build this page was served by.
const PAGE_VERSION = document.querySelector('meta[name="agentlink-version"]')?.content || "";
const CORE_SLICES = new Set(["status", "dashboard", "participants", "update", "settings", "chats"]);
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

const store = createStore({
  status: null, dashboard: null, participants: null, update: null, settings: null,
  chats: null, chatArchive: null, showArchive: false, selectedChat: "", selectedMessage: "", drafts: {},
});

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

// reloadOnNewVersion reloads the page when the app answering is another build,
// as after a self-update: the old page cannot use the new app's token. A
// reconnect to the same build keeps the page.
function reloadOnNewVersion(resp) {
  const version = resp?.headers?.get?.("X-Agentlink-Version") || "";
  if (!PAGE_VERSION || !version || version === PAGE_VERSION) return false;
  location.reload();
  return true;
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
  if (reloadOnNewVersion(resp)) throw new Error(t("error.no_app"));
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
const coreFailures = new Map();

function showConnectionProblem(name, error) {
  coreFailures.set(name, error);
  const region = document.getElementById("connection-banner") || document.getElementById("toast-region");
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

function clearConnectionProblem(name) {
  coreFailures.delete(name);
  if (!reloadRequired && !coreFailures.size) (document.getElementById("connection-banner") || document.getElementById("toast-region"))?.replaceChildren();
}

function refreshSlice(name) {
  if (name === "chats") {
    // The archive is read only while it is on screen; the main list always,
    // since message notifications come from it.
    const archive = store.get().showArchive ? api("GET", "chats?archive=1") : null;
    return Promise.all([api("GET", "chats"), archive]).then(([main, archived]) => {
      if (archived) store.patch("chatArchive", archived);
      store.patch("chats", main);
      clearConnectionProblem(name);
    }).catch((error) => showConnectionProblem(name, error));
  }
  return api("GET", name).then((value) => {
    store.patch(name, value);
    clearConnectionProblem(name);
  }).catch((error) => showConnectionProblem(name, error));
}

function slicesForTopics(topics) {
  if (topics.includes("all")) return [...CORE_SLICES];
  const slices = new Set();
  for (const topic of topics) {
    if (CORE_SLICES.has(topic)) slices.add(topic);
    if (topic === "peer" || topic === "members") {
      slices.add("status"); slices.add("dashboard"); slices.add("participants");
    }
    if (topic === "messages" || topic === "worker") {
      slices.add("dashboard"); slices.add("participants"); slices.add("chats");
    }
  }
  return [...slices];
}

function applyChange(event) {
  const topics = Array.isArray(event.topics) ? event.topics : [];
  return Promise.all(slicesForTopics(topics).map(refreshSlice));
}

function parseSSERecord(record) {
  const lines = record.split(/\r?\n/);
  if (!lines.some((line) => line === "event: change")) return null;
  const data = lines.filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
  if (!data) return null;
  try { return JSON.parse(data); } catch (_) { return null; }
}

let reconnectDelay = 500;
let reconnectTimer = null;

function scheduleReconnect() {
  if (reloadRequired || reconnectTimer !== null) return;
  const delay = reconnectDelay;
  reconnectDelay = Math.min(reconnectDelay * 2, 10000);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connectEvents();
  }, delay);
}

async function connectEvents() {
  let response;
  try {
    response = await fetch("/ui/api/events", { headers: { "X-Agentlink-Token": TOKEN } });
    if (reloadOnNewVersion(response)) return;
    if (response.status === 403) {
      const error = new Error(t("error.forbidden"));
      error.status = 403;
      showConnectionProblem("events", error);
      return;
    }
    if (!response.ok || !response.body) throw new Error(t("error.no_app"));
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) throw new Error(t("error.no_app"));
      buffer += decoder.decode(value, { stream: true });
      let split;
      while ((split = buffer.search(/\r?\n\r?\n/)) >= 0) {
        const record = buffer.slice(0, split);
        const separator = buffer.slice(split).match(/^\r?\n\r?\n/)[0];
        buffer = buffer.slice(split + separator.length);
        const event = parseSSERecord(record);
        if (event) {
          reconnectDelay = 500;
          clearConnectionProblem("events");
          await applyChange(event);
        }
      }
    }
  } catch (error) {
    showConnectionProblem("events", error);
    scheduleReconnect();
  }
}

applyStrings();
store.subscribe("status", renderStatus);
connectEvents();
