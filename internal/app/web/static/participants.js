"use strict";

async function renderParticipants() {
  const list = document.getElementById("participants");
  const empty = document.getElementById("participants_empty");
  try {
    const people = await api("GET", "participants");
    list.replaceChildren(...people.map((person) => {
      const row = document.createElement("li");
      const name = document.createElement("strong");
      name.textContent = person.name;
      row.append(name);
      const detail = document.createElement("span");
      detail.className = person.online ? "on" : "off";
      detail.textContent = t(person.online ? "settings.members.online" : "settings.members.lost");
      row.append(" ", detail);
      return row;
    }));
    empty.hidden = people.length !== 0;
  } catch (e) {
    list.replaceChildren();
    empty.textContent = e.message;
    empty.hidden = false;
  }
}
