"use strict";

const DASHBOARD_WINDOW_NAME = "agentlink-dashboard";
const DASHBOARD_PATH = "/ui/dashboard";
const DASHBOARD_ACTIVATION_KEY = "agentlink" + ".dashboard.activate";
const DASHBOARD_CHANNEL = "agentlink" + ".dashboard";

function launchDashboard() {
  let target;
  try {
    target = window.open(DASHBOARD_PATH, DASHBOARD_WINDOW_NAME);
  } catch (_) {
    adoptLauncher();
    return;
  }
  if (!target) {
    adoptLauncher();
    return;
  }
  try { target.focus(); } catch (_) { /* focus remains browser-controlled */ }
  signalDashboard();
  try { window.close(); } catch (_) { /* browser may keep the launcher tab */ }
}

function signalDashboard() {
  const message = { route: "dashboard", at: Date.now(), nonce: Math.random().toString(36).slice(2) };
  try { localStorage.setItem(DASHBOARD_ACTIVATION_KEY, JSON.stringify(message)); } catch (_) { /* storage unavailable */ }
  if (typeof BroadcastChannel !== "undefined") {
    try {
      const channel = new BroadcastChannel(DASHBOARD_CHANNEL);
      channel.postMessage(message);
      channel.close();
    } catch (_) { /* channel unavailable */ }
  }
}

function adoptLauncher() {
  window.name = DASHBOARD_WINDOW_NAME;
  location.replace(DASHBOARD_PATH);
}

launchDashboard();
