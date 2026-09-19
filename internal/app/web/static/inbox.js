"use strict";

const list = document.getElementById("messages");
const conversationList = document.getElementById("conversation_list");
const sendForm = document.getElementById("send");
const sendButton = document.getElementById("send_button");
const sendResult = document.getElementById("inbox_result");
const replyTo = document.getElementById("reply_to");
const recipient = document.getElementById("to");
const composer = document.getElementById("body");
const messageNodes = new Map();
let sending = false;
let notificationNode = "";
let notificationIDs = null;

if (composer) composer.placeholder = t("inbox.body.placeholder");

function setReply(id, from) {
  replyTo.value = id || "";
  document.getElementById("replying").hidden = !id;
  document.getElementById("replying_text").textContent = id ? fmt("inbox.replying", { id: id.slice(0, 8) }) : "";
  if (from) recipient.value = from;
  if (id) composer.focus();
}

function when(iso) { return new Date(iso).toLocaleString("ru-RU"); }
function statusText(status) { return fmt("inbox.status", { status: t("status." + status) }); }

function block(labelKey, value, cls) {
  const wrap = document.createElement("div");
  wrap.className = cls;
  const label = document.createElement("span");
  label.className = "tag";
  label.textContent = t(labelKey);
  const body = document.createElement("pre");
  body.textContent = value || "";
  wrap.append(label, body);
  return wrap;
}

function patchMessageNode(node, thread) {
  node.className = "message " + thread.direction;
  node.dataset.messageId = thread.id;
  node.tabIndex = -1;
  const head = document.createElement("div");
  head.className = "head";
  const direction = document.createElement("strong");
  direction.textContent = t(thread.direction === "in" ? "inbox.dir.in" : "inbox.dir.out");
  const parts = [fmt("inbox.route", { from: thread.from, to: thread.to })];
  if (thread.area) parts.push(fmt("inbox.area", { area: thread.area }));
  parts.push(when(thread.created_at), statusText(thread.status));
  head.append(direction, document.createTextNode(" · " + parts.join(" · ")));
  const children = [head, block("inbox.question", thread.body, "question")];
  if (thread.answered) {
    children.push(block("inbox.answer", thread.answer, "answer"));
    if (thread.answer_at) {
      const answerAt = document.createElement("div");
      answerAt.className = "head";
      answerAt.textContent = when(thread.answer_at);
      children.push(answerAt);
    }
  } else {
    const pending = document.createElement("p");
    pending.className = "hint";
    pending.textContent = t("inbox.no_answer");
    children.push(pending);
    if (thread.activity) {
      const activity = document.createElement("p");
      activity.className = "activity";
      activity.textContent = fmt("inbox.activity", { activity: thread.activity });
      children.push(activity);
    }
    if (thread.no_news_min) {
      const silent = document.createElement("p");
      silent.className = "warn";
      silent.textContent = fmt("inbox.no_news", { min: String(thread.no_news_min) });
      children.push(silent);
    }
  }
  if (thread.replyable) {
    const reply = document.createElement("button");
    reply.type = "button";
    reply.textContent = t("inbox.reply");
    reply.addEventListener("click", () => setReply(thread.id, thread.from));
    children.push(reply);
  }
  node.replaceChildren(...children);
}

function renderTimeline(items) {
  if (!Array.isArray(items)) return;
  const active = document.activeElement;
  const selectionStart = active === composer ? composer.selectionStart : null;
  const selectionEnd = active === composer ? composer.selectionEnd : null;
  const oldScroll = list.scrollTop;
  const nearBottom = list.scrollHeight - list.clientHeight - list.scrollTop <= 80;
  const ordered = [...items].sort((a, b) => new Date(a.created_at) - new Date(b.created_at));
  const nextIDs = new Set(ordered.map((thread) => thread.id));
  for (const [id, node] of messageNodes) {
    if (!nextIDs.has(id)) {
      node.remove();
      messageNodes.delete(id);
    }
  }
  const nodes = ordered.map((thread) => {
    let node = messageNodes.get(thread.id);
    if (!node) {
      node = document.createElement("li");
      messageNodes.set(thread.id, node);
    }
    patchMessageNode(node, thread);
    return node;
  });
  if (!nodes.length) {
    const empty = document.createElement("li");
    empty.className = "empty";
    empty.textContent = store.get().selectedPeer ? t("inbox.empty_conversation") : t("inbox.select_hint");
    list.replaceChildren(empty);
  } else list.replaceChildren(...nodes);
  if (active === composer) {
    composer.focus({ preventScroll: true });
    if (selectionStart !== null && typeof composer.setSelectionRange === "function") composer.setSelectionRange(selectionStart, selectionEnd);
    else { composer.selectionStart = selectionStart; composer.selectionEnd = selectionEnd; }
  }
  const anchor = store.get().selectedMessage;
  const target = anchor ? messageNodes.get(anchor) : null;
  if (target) {
    target.focus({ preventScroll: true });
    target.scrollIntoView({ block: "center" });
    store.patch("selectedMessage", "");
  } else if (nearBottom) list.scrollTop = list.scrollHeight;
  else list.scrollTop = oldScroll;
}

function conversationPeers() {
  const peers = new Map();
  for (const person of store.get().participants || []) peers.set(person.name, person);
  for (const recent of store.get().dashboard?.recent || []) {
    if (!peers.has(recent.peer)) peers.set(recent.peer, { name: recent.peer, online: false, total: 0 });
  }
  const selected = store.get().selectedPeer;
  if (selected && !peers.has(selected)) peers.set(selected, { name: selected, online: false, total: 0 });
  return [...peers.values()];
}

function renderConversationList() {
  const selected = store.get().selectedPeer;
  const rows = conversationPeers().map((person) => {
    const row = document.createElement("li");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "conversation-choice" + (person.name === selected ? " active" : "");
    button.setAttribute("aria-pressed", person.name === selected ? "true" : "false");
    const name = document.createElement("strong");
    name.textContent = person.name;
    const meta = document.createElement("span");
    meta.textContent = fmt(person.online ? "inbox.peer_online" : "inbox.peer_offline", { total: person.total || 0 });
    button.append(name, meta);
    button.addEventListener("click", () => navigate("inbox", { peer: person.name }));
    row.append(button);
    return row;
  });
  conversationList.replaceChildren(...rows);
}

function saveDraft(peer) {
  if (peer) store.patch("drafts", Object.assign({}, store.get().drafts, { [peer]: composer.value }));
}

async function selectConversation(peer, messageID) {
  const previous = store.get().selectedPeer;
  if (previous && previous !== peer) saveDraft(previous);
  store.patch("selectedPeer", peer || "");
  store.patch("selectedMessage", messageID || "");
  recipient.value = peer || "";
  composer.value = store.get().drafts[peer] || "";
  setReply("");
  const title = document.getElementById("conversation_title");
  if (title) title.textContent = peer || t("inbox.select");
  renderConversationList();
  if (!peer) { renderTimeline(store.get().threadFeed || []); return; }
  try {
    const selected = await api("GET", "threads?peer=" + encodeURIComponent(peer));
    if (store.get().selectedPeer === peer) store.patch("threads", selected);
  } catch (error) { sendResult.textContent = error.message; }
}

async function submitMessage() {
  if (sending) return;
  const peer = store.get().selectedPeer || recipient.value.trim();
  const draft = composer.value;
  sending = true;
  sendButton.disabled = true;
  sendForm.setAttribute("aria-busy", "true");
  try {
    const sent = await api("POST", "send", { to: peer, body: draft, reply_to: replyTo.value });
    const current = store.get().threads || [];
    const optimistic = { id: sent.id, direction: "out", from: store.get().status?.node || "", to: peer, body: sent.body || draft, created_at: sent.created_at || new Date().toISOString(), status: sent.status || "queued", answered: false, replyable: false };
    store.patch("threads", current.some((item) => item.id === optimistic.id) ? current : current.concat(optimistic));
    store.patch("drafts", Object.assign({}, store.get().drafts, { [peer]: "" }));
    if (store.get().selectedPeer === peer) composer.value = "";
    setReply("");
    sendResult.textContent = t("inbox.sent");
    if (store.get().selectedPeer === peer) {
      const refreshed = await api("GET", "threads?peer=" + encodeURIComponent(peer));
      if (store.get().selectedPeer === peer) store.patch("threads", refreshed);
    }
  } catch (error) { sendResult.textContent = error.message; }
  finally { sending = false; sendButton.disabled = false; sendForm.removeAttribute("aria-busy"); }
}

function detectIncoming(previousIDs, items) {
  const previous = new Set(previousIDs || []);
  return incomingNotifications(items).filter((message) => !previous.has(message.notificationID));
}

function notificationKey(node) { return "agentlink.notifications.v1:" + node; }
function incomingNotifications(items) {
  const messages = [];
  for (const thread of items || []) {
    if (thread.direction === "in" && thread.id) {
      messages.push(Object.assign({}, thread, { notificationID: thread.id, focusID: thread.id, peer: thread.from, notificationAt: thread.created_at }));
    } else if (thread.direction === "out" && thread.id && thread.answered && thread.answer_at) {
      messages.push(Object.assign({}, thread, {
        notificationID: "reply:" + thread.id + ":" + thread.answer_at,
        focusID: thread.id, peer: thread.to, from: thread.to, body: thread.answer,
        notificationAt: thread.answer_at,
      }));
    }
  }
  return messages;
}
function incomingIDs(items) {
  return incomingNotifications(items).sort((a, b) => new Date(b.notificationAt) - new Date(a.notificationAt))
    .map((message) => message.notificationID);
}
function persistNotificationIDs(node, ids) {
  const bounded = [...new Set(ids)].slice(0, 500);
  try { localStorage.setItem(notificationKey(node), JSON.stringify(bounded)); } catch (_) { /* storage may be unavailable */ }
  notificationIDs = bounded;
}

function showMessageToast(thread) {
  const region = document.getElementById("message-toast-region");
  if (!region) return;
  while (region.childElementCount >= 3 && region.firstElementChild) region.firstElementChild.remove();
  const toast = document.createElement("button");
  toast.type = "button";
  toast.className = "message-toast";
  const sender = document.createElement("strong");
  sender.textContent = thread.from;
  const preview = document.createElement("span");
  preview.className = "message-toast-preview";
  const chars = Array.from(String(thread.body || "").replace(/\s+/g, " ").trim());
  preview.textContent = chars.slice(0, 120).join("") + (chars.length > 120 ? "…" : "");
  toast.append(sender, preview);
  toast.addEventListener("click", () => navigate("inbox", { peer: thread.peer || thread.from, message: thread.focusID || thread.id }));
  region.append(toast);
}

function processIncomingThreads(items) {
  const node = store.get().status?.node;
  if (!node) return;
  if (notificationNode !== node) {
    notificationNode = node;
    let saved = null;
    try { saved = localStorage.getItem(notificationKey(node)); } catch (_) { /* storage may be unavailable */ }
    if (saved === null) { persistNotificationIDs(node, incomingIDs(items)); return; }
    try { notificationIDs = JSON.parse(saved); } catch (_) { notificationIDs = []; }
    if (!Array.isArray(notificationIDs)) notificationIDs = [];
  }
  const unseen = detectIncoming(notificationIDs, items);
  persistNotificationIDs(node, incomingIDs(items).concat(notificationIDs || []));
  for (const thread of unseen.slice(0, 3)) showMessageToast(thread);
}

document.getElementById("cancel_reply").addEventListener("click", () => setReply(""));
composer.addEventListener("input", () => saveDraft(store.get().selectedPeer));
sendForm.addEventListener("submit", (event) => { event.preventDefault(); submitMessage(); });
store.subscribe("threads", renderTimeline);
store.subscribe("threadFeed", processIncomingThreads);
store.subscribe("participants", renderConversationList);
store.subscribe("dashboard", renderConversationList);
store.subscribe("status", (status) => {
  renderConversationList();
  if (store.get().threadFeed) processIncomingThreads(store.get().threadFeed);
  if (!store.get().selectedPeer && status.peer) {
    recipient.value = status.peer;
    const inboxOpen = typeof routeFromPath !== "function" || routeFromPath(location.pathname) === "inbox";
    if (inboxOpen) selectConversation(status.peer, "");
  }
});
if (store.get().threads) renderTimeline(store.get().threads);
renderConversationList();
