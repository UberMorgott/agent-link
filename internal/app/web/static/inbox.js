"use strict";

const list = document.getElementById("messages");
const sendForm = document.getElementById("send");
const sendResult = document.getElementById("result");
const replyTo = document.getElementById("reply_to");

function setReply(id, from) {
  replyTo.value = id || "";
  document.getElementById("replying").hidden = !id;
  document.getElementById("reply_id").textContent = id || "";
  if (from) document.getElementById("to").value = from;
  if (id) document.getElementById("body").focus();
}

function render(entries) {
  list.replaceChildren();
  for (const e of entries) {
    const li = document.createElement("li");
    li.className = e.direction;
    const head = document.createElement("div");
    head.className = "head";
    const who = e.direction === "in" ? "from " + e.from : "to " + (e.area ? "area:" + e.area + " / " : "") + e.peer;
    head.textContent = new Date(e.created_at).toLocaleString() + " - " + who + " - " + e.status +
      (e.job_status ? " - " + e.job_status : "") +
      (e.reply_to ? " - reply to " + e.reply_to.slice(0, 8) : "");
    const body = document.createElement("pre");
    body.textContent = e.body;
    li.append(head, body);
    if (e.direction === "out" && e.answer) {
      const answer = document.createElement("pre");
      answer.className = "answer";
      answer.textContent = e.answer;
      li.append(answer);
    }
    if (e.direction === "in" && !e.reply_to) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.textContent = "Reply";
      btn.addEventListener("click", () => setReply(e.id, e.from));
      li.append(btn);
    }
    list.append(li);
  }
}

async function refresh() {
  try {
    render(await api("GET", "inbox"));
  } catch (e) {
    sendResult.textContent = "Could not load messages: " + e.message;
  }
}

document.getElementById("cancel_reply").addEventListener("click", () => setReply(""));

sendForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const body = {
    to: document.getElementById("to").value.trim(),
    body: document.getElementById("body").value,
    reply_to: replyTo.value,
  };
  try {
    await api("POST", "send", body);
    document.getElementById("body").value = "";
    setReply("");
    sendResult.textContent = "Queued.";
    refresh();
  } catch (e) {
    sendResult.textContent = "Not sent: " + e.message;
  }
});

api("GET", "settings").then((s) => {
  const to = document.getElementById("to");
  if (!to.value) to.value = s.peer_name || "";
}).catch(() => {});

refresh();
setInterval(refresh, 3000);
