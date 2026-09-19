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
if (recipient) recipient.placeholder = t("inbox.to.placeholder");

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

function createMessageNode() {
  const node = document.createElement("li");
  const head = document.createElement("div");
  head.className = "head";
  const direction = document.createElement("strong");
  const meta = document.createTextNode("");
  head.append(direction, meta);
  const question = block("inbox.question", "", "question");
  const answer = block("inbox.answer", "", "answer");
  const answerAt = document.createElement("div");
  answerAt.className = "head";
  const pending = document.createElement("p");
  pending.className = "hint";
  const activity = document.createElement("p");
  activity.className = "activity";
  const silent = document.createElement("p");
  silent.className = "warn";
  const reply = document.createElement("button");
  reply.type = "button";
  reply.textContent = t("inbox.reply");
  reply.addEventListener("click", () => setReply(node._thread.id, node._thread.from));
  node.append(head, question, answer, answerAt, pending, activity, silent, reply);
  node._parts = { direction, meta, questionBody: question.children[1], answer, answerBody: answer.children[1], answerAt, pending, activity, silent, reply };
  return node;
}

function threadVersion(thread) {
  return JSON.stringify([
    thread.direction, thread.from, thread.to, thread.area, thread.body, thread.created_at,
    thread.status, thread.answer, thread.answer_at, thread.answered, thread.replyable,
    thread.activity, thread.no_news_min,
  ]);
}

function patchMessageNode(node, thread) {
  const version = threadVersion(thread);
  if (node._version === version) return;
  node._version = version;
  node._thread = thread;
  node.className = "message " + thread.direction;
  node.dataset.messageId = thread.id;
  node.tabIndex = -1;
  const refs = node._parts;
  refs.direction.textContent = t(thread.direction === "in" ? "inbox.dir.in" : "inbox.dir.out");
  const metadata = [fmt("inbox.route", { from: thread.from, to: thread.to })];
  if (thread.area) metadata.push(fmt("inbox.area", { area: thread.area }));
  metadata.push(when(thread.created_at), statusText(thread.status));
  refs.meta.textContent = " · " + metadata.join(" · ");
  node._parts.questionBody.textContent = thread.body || "";
  node._parts.answer.hidden = !thread.answered;
  node._parts.answerBody.textContent = thread.answer || "";
  node._parts.answerAt.hidden = !thread.answer_at;
  node._parts.answerAt.textContent = thread.answer_at ? when(thread.answer_at) : "";
  node._parts.pending.hidden = Boolean(thread.answered);
  node._parts.pending.textContent = thread.answered ? "" : t("inbox.no_answer");
  node._parts.activity.hidden = Boolean(thread.answered || !thread.activity);
  node._parts.activity.textContent = thread.activity ? fmt("inbox.activity", { activity: thread.activity }) : "";
  node._parts.silent.hidden = Boolean(thread.answered || !thread.no_news_min);
  node._parts.silent.textContent = thread.no_news_min ? fmt("inbox.no_news", { min: String(thread.no_news_min) }) : "";
  node._parts.reply.hidden = !thread.replyable;
}

function reconcileTimeline(nodes) {
  for (let index = 0; index < nodes.length; index++) {
    if (list.children[index] !== nodes[index]) list.insertBefore(nodes[index], list.children[index] || null);
  }
  while (list.children.length > nodes.length) list.lastElementChild.remove();
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
      node = createMessageNode();
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
  } else reconcileTimeline(nodes);
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
    if (!peers.has(recent.peer)) {
      peers.set(recent.peer, {
        name: recent.peer, online: false, total: 0, latest_preview: recent.preview,
        latest_at: recent.latest_at, latest_direction: recent.direction,
      });
    }
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
    const preview = document.createElement("span");
    preview.className = "conversation-preview";
    preview.textContent = person.latest_preview || "";
    const foot = document.createElement("span");
    foot.className = "conversation-foot";
    const timestamp = document.createElement("span");
    timestamp.textContent = person.latest_at ? when(person.latest_at) : "";
    const unread = document.createElement("span");
    unread.className = "conversation-unread";
    const readAt = store.get().conversationReads[person.name] || "";
    unread.hidden = !(person.latest_direction === "in" && person.name !== selected && (!readAt || new Date(person.latest_at) > new Date(readAt)));
    unread.textContent = unread.hidden ? "" : t("inbox.unread");
    foot.append(timestamp, unread);
    button.append(name, meta, preview, foot);
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
  if (peer) store.patch("conversationReads", Object.assign({}, store.get().conversationReads, { [peer]: new Date().toISOString() }));
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
  const selectedPeer = store.get().selectedPeer;
  const peer = recipient.value.trim() || selectedPeer;
  const draft = composer.value;
  sending = true;
  sendButton.disabled = true;
  sendForm.setAttribute("aria-busy", "true");
  try {
    const sent = await api("POST", "send", { to: peer, body: draft, reply_to: replyTo.value });
    const current = store.get().threads || [];
    const optimistic = { id: sent.id, direction: "out", from: store.get().status?.node || "", to: peer, body: sent.body || draft, created_at: sent.created_at || new Date().toISOString(), status: sent.status || "queued", answered: false, replyable: false };
    store.patch("threads", current.some((item) => item.id === optimistic.id) ? current : current.concat(optimistic));
    const draftPeer = selectedPeer || peer;
    store.patch("drafts", Object.assign({}, store.get().drafts, { [draftPeer]: "" }));
    if (store.get().selectedPeer === selectedPeer) composer.value = "";
    setReply("");
    sendResult.textContent = t("inbox.sent");
    if (selectedPeer && store.get().selectedPeer === selectedPeer) {
      const refreshed = await api("GET", "threads?peer=" + encodeURIComponent(selectedPeer));
      if (store.get().selectedPeer === selectedPeer) store.patch("threads", refreshed);
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
