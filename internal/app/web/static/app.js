"use strict";

const ROUTES = new Set(["dashboard", "inbox", "participants", "settings"]);

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
  if (route === "inbox") {
    const peer = typeof state.query.peer === "string" ? state.query.peer : store.get().selectedPeer;
    const message = typeof state.query.message === "string" ? state.query.message : "";
    selectConversation(peer || store.get().status?.peer || "", message);
  }
  document.getElementById("view").focus({ preventScroll: true });
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
