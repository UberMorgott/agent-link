"use strict";

const DASHBOARD_WINDOW_NAME = "agentlink-dashboard";
const DASHBOARD_PATH = "/ui/dashboard";
const DASHBOARD_HEARTBEAT_KEY = "agentlink" + ".dashboard.heartbeat";
const DASHBOARD_ACTIVATION_KEY = "agentlink" + ".dashboard.activate";
const DASHBOARD_CHANNEL = "agentlink" + ".dashboard";

function launcherDecision(now, heartbeat) {
  const seen = Number(heartbeat);
  return Number.isFinite(seen) && heartbeat !== null && now - seen >= 0 && now - seen < 5000
    ? "activate"
    : "adopt";
}

function launchDashboard(environment) {
  const decision = launcherDecision(environment.now(), environment.heartbeat());
  if (decision === "adopt") {
    environment.adopt();
    return "adopt";
  }
  let target;
  try {
    target = environment.open();
  } catch (_) {
    environment.adopt();
    return "fallback";
  }
  if (!target) {
    environment.adopt();
    return "fallback";
  }
  try {
    target.focus();
  } catch (_) {
    environment.adopt();
    return "fallback";
  }
  environment.signal();
  try {
    environment.close();
  } catch (_) {
    environment.adopt();
    return "fallback";
  }
  environment.afterClose(() => {
    if (!environment.closed()) environment.adopt();
  });
  return "activate";
}

function storageRead(key) {
  try { return localStorage.getItem(key); } catch (_) { return null; }
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

function runLauncher() {
  launchDashboard({
    now: () => Date.now(),
    heartbeat: () => storageRead(DASHBOARD_HEARTBEAT_KEY),
    open: () => window.open(DASHBOARD_PATH, DASHBOARD_WINDOW_NAME),
    signal: signalDashboard,
    close: () => window.close(),
    closed: () => window.closed,
    afterClose: (callback) => setTimeout(callback, 100),
    adopt: adoptLauncher,
  });
}

if (typeof module !== "undefined" && module.exports) module.exports = { launcherDecision, launchDashboard };
if (typeof window !== "undefined" && typeof document !== "undefined") runLauncher();
