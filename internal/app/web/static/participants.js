"use strict";

const participantList = document.getElementById("participants");
const participantResult = document.getElementById("participants_result");
const participantAddr = document.getElementById("participant_addr");

function participantDetail(person) {
  const details = [];
  if (!person.online && person.seen) details.push(fmt("participants.seen", { when: new Date(person.seen).toLocaleString("ru-RU") }));
  if (person.app) details.push(fmt("participants.version", { version: person.app }));
  if (person.old_auth) details.push(t("participants.old_auth"));
  else if (person.legacy) details.push(t("participants.legacy"));
  if ((person.addrs || []).length) details.push(fmt("participants.addresses", { addresses: person.addrs.join(", ") }));
  return details;
}

function renderParticipants(items) {
  const empty = document.getElementById("participants_empty");
  if (!Array.isArray(items)) return;
  participantList.replaceChildren(...items.map((person) => {
    const row = document.createElement("li");
    row.className = "participant-card";

    const open = document.createElement("button");
    open.type = "button";
    open.className = "participant-main";
    open.setAttribute("aria-label", fmt("participants.open_chat", { name: person.name }));
    open.addEventListener("click", () => navigate("inbox", { peer: person.name }));

    const heading = document.createElement("span");
    heading.className = "participant-heading";
    const name = document.createElement("strong");
    name.textContent = person.name;
    const state = document.createElement("span");
    state.className = person.online ? "on" : "off";
    state.textContent = t(person.online ? "participants.online" : "participants.lost");
    heading.append(name, state);

    const counts = document.createElement("span");
    counts.className = "participant-counts";
    counts.textContent = fmt("participants.counts", {
      sent: person.sent || 0, received: person.received || 0, total: person.total || 0,
    });
    open.append(heading, counts);
    const details = participantDetail(person);
    if (details.length) {
      const detail = document.createElement("span");
      detail.className = "participant-detail";
      detail.textContent = details.join(" · ");
      open.append(detail);
    }

    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "danger participant-remove";
    remove.textContent = t("participants.remove");
    remove.addEventListener("click", () => removeParticipant(person.name, row));
    row.append(open, remove);
    return row;
  }));
  empty.hidden = items.length !== 0;
}

async function refreshParticipantViews() {
  await Promise.all([refreshSlice("participants"), refreshSlice("dashboard")]);
}

async function addParticipant(addr) {
  const value = String(addr || "").trim();
  if (!value) {
    participantResult.textContent = t("participants.add.empty");
    return;
  }
  try {
    const status = await api("POST", "members/add", { addr: value });
    store.patch("status", status);
    participantAddr.value = "";
    participantResult.textContent = fmt("participants.add.added", { addr: value });
    await refreshParticipantViews();
  } catch (e) {
    participantResult.textContent = e.message;
  }
}

async function removeParticipant(name, row) {
  if (!confirm(fmt("participants.confirm", { name }))) return;
  const rows = Array.from(participantList.children);
  const index = Math.max(0, rows.indexOf(row));
  try {
    const status = await api("POST", "members/remove", { name });
    store.patch("status", status);
    participantResult.textContent = fmt("participants.removed", { name });
    await refreshParticipantViews();
    const next = participantList.querySelectorAll(".participant-main");
    if (next.length) next[Math.min(index, next.length - 1)].focus();
    else participantAddr.focus();
  } catch (e) {
    participantResult.textContent = e.message;
  }
}

document.getElementById("participant_add").addEventListener("submit", (event) => {
  event.preventDefault();
  addParticipant(participantAddr.value);
});

store.subscribe("participants", renderParticipants);
if (store.get().participants) renderParticipants(store.get().participants);
