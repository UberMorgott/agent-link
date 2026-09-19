"use strict";

async function renderOverview() {
  const summary = document.getElementById("dashboard_summary");
  try {
    const data = await api("GET", "dashboard");
    summary.textContent = fmt("dashboard.summary", {
      online: data.status.online || 0,
      total: data.status.total || 0,
      messages: data.total_messages || 0,
    });
  } catch (e) {
    summary.textContent = e.message;
  }
}
