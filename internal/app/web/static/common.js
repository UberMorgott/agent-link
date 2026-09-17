"use strict";

const TOKEN = document.querySelector('meta[name="agentlink-token"]').content;

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
    if (!s.configured) { text = "not configured"; cls = "off"; }
    else if (s.error) { text = "error: " + s.error; cls = "off"; }
    else if (s.connected) { text = s.peer + " connected"; cls = "on"; }
    else { text = s.peer + " offline"; cls = "off"; }
    el.textContent = text;
    el.className = cls;
  } catch (e) {
    el.textContent = "agentlink is not running";
    el.className = "off";
  }
}

refreshStatus();
setInterval(refreshStatus, 3000);
