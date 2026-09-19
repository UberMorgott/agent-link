"use strict";

function overviewCard(label, value) {
  const card = document.createElement("article");
  const title = document.createElement("h2");
  title.textContent = label;
  const text = document.createElement("p");
  text.textContent = value;
  card.append(title, text);
  return card;
}

function renderOverview(data) {
  const summary = document.getElementById("dashboard_summary");
  if (!data) return;
  const status = data.status || {};
  summary.textContent = fmt("dashboard.summary", {
    online: status.online || 0,
    total: status.total || 0,
    messages: data.total_messages || 0,
  });
  document.getElementById("dashboard_cards").replaceChildren(
    overviewCard(t("dashboard.online"), fmt("link.on_many", { online: status.online || 0, total: status.total || 0 })),
    overviewCard(t("dashboard.messages"), String(data.total_messages || 0)),
    overviewCard(t("dashboard.active"), String(data.active_requests || 0)),
    overviewCard(t("dashboard.handler"), t("settings.handler." + (status.handler || "none"))),
  );
  const recent = document.getElementById("dashboard_recent");
  recent.replaceChildren();
  for (const conversation of data.recent || []) {
    const item = document.createElement("li");
    const name = document.createElement("strong");
    name.textContent = conversation.peer || "—";
    const preview = document.createElement("span");
    preview.textContent = conversation.preview || "";
    item.append(name, document.createTextNode(": "), preview);
    recent.append(item);
  }
  if (!recent.childElementCount) {
    const item = document.createElement("li");
    item.textContent = t("dashboard.recent.empty");
    recent.append(item);
  }
}

store.subscribe("dashboard", renderOverview);
