"use strict";

const ROUTES = new Set(["dashboard", "inbox", "participants", "settings"]);
const DASHBOARD_WINDOW_NAME = "agentlink-dashboard";
const DASHBOARD_HEARTBEAT_KEY = "agentlink" + ".dashboard.heartbeat";
const DASHBOARD_ACTIVATION_KEY = "agentlink" + ".dashboard.activate";
const DASHBOARD_CHANNEL = "agentlink" + ".dashboard";

window.name = DASHBOARD_WINDOW_NAME;
// The inbox shows the sections as an icon rail: the name is the tooltip.
for (const link of document.querySelectorAll("#sidebar nav a")) link.title = link.textContent.trim();

function writeDashboardHeartbeat() {
  try { localStorage.setItem(DASHBOARD_HEARTBEAT_KEY, String(Date.now())); } catch (_) { /* storage unavailable */ }
}

function activateDashboard(message) {
  if (!message || !ROUTES.has(message.route)) return;
  navigate(message.route);
  try { window.focus(); } catch (_) { /* focus remains browser-controlled */ }
}

function parseActivation(raw) {
  try { return JSON.parse(raw); } catch (_) { return null; }
}

writeDashboardHeartbeat();
setInterval(writeDashboardHeartbeat, 1000);
document.addEventListener("visibilitychange", writeDashboardHeartbeat);
addEventListener("storage", (event) => {
  if (event.key === DASHBOARD_ACTIVATION_KEY && event.newValue) activateDashboard(parseActivation(event.newValue));
});
if (typeof BroadcastChannel !== "undefined") {
  try {
    const dashboardChannel = new BroadcastChannel(DASHBOARD_CHANNEL);
    dashboardChannel.addEventListener("message", (event) => activateDashboard(event.data));
  } catch (_) { /* channel unavailable */ }
}

function routeFromPath(pathname) {
  const route = pathname.replace(/^\/ui\/?/, "").split("/")[0];
  return ROUTES.has(route) ? route : "dashboard";
}

function navigate(route, query) {
  if (!ROUTES.has(route)) route = "dashboard";
  const search = query ? "?" + new URLSearchParams(query) : "";
  history.pushState(null, "", "/ui/" + route + search);
  renderRoute({ route, query: query || {} });
}

function renderRoute(state) {
  const route = ROUTES.has(state.route) ? state.route : "dashboard";
  for (const view of document.querySelectorAll("[data-view]")) view.hidden = view.dataset.view !== route;
  for (const link of document.querySelectorAll("[data-route]")) link.classList.toggle("active", link.dataset.route === route);
  document.title = t("page.title." + route);
  if (route === "dashboard") renderOverview(store.get().dashboard);
  if (route === "participants") renderParticipants();
  if (route === "inbox") openInbox(state.query);
  const view = document.getElementById("view");
  view.dataset.route = route;
  view.focus({ preventScroll: true });
}

document.addEventListener("click", (event) => {
  const link = event.target.closest('a[data-route]');
  if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || link.target) return;
  const url = new URL(link.href, location.href);
  if (url.origin !== location.origin || !url.pathname.startsWith("/ui/")) return;
  event.preventDefault();
  navigate(routeFromPath(url.pathname), Object.fromEntries(url.searchParams));
});

addEventListener("popstate", () => renderRoute({ route: routeFromPath(location.pathname), query: Object.fromEntries(new URLSearchParams(location.search)) }));
renderRoute({ route: routeFromPath(location.pathname), query: Object.fromEntries(new URLSearchParams(location.search)) });
