"use strict";

// The inbox: a list of chats on the left, the open chat on the right. Chats,
// their messages and the live activity of every participant come from
// /ui/api/chats; SSE "chats"/"messages" events reload them (common.js).

const layout = document.getElementById("conversation_layout");
const conversationList = document.getElementById("conversation_list");
const archiveToggle = document.getElementById("archive_toggle");
const newChatButton = document.getElementById("new_chat");
const list = document.getElementById("messages");
const chatTitle = document.getElementById("conversation_title");
const chatSubtitle = document.getElementById("chat_subtitle");
const chatMembers = document.getElementById("chat_members");
const chatCloseButton = document.getElementById("chat_close");
const chatBack = document.getElementById("chat_back");
const activityDock = document.getElementById("chat_activity");
const sendForm = document.getElementById("send");
const sendButton = document.getElementById("send_button");
const sendResult = document.getElementById("inbox_result");
const askChoices = document.getElementById("ask_choices");
const askHint = document.getElementById("ask_hint");
const replyTo = document.getElementById("reply_to");
const composer = document.getElementById("body");
const chatNote = document.getElementById("chat_note");
const chatNoteText = document.getElementById("chat_note_text");
const chatNoteAction = document.getElementById("chat_note_action");
const newChatForm = document.getElementById("new_chat_form");
const newChatMembers = document.getElementById("new_chat_members");
const newChatEmpty = document.getElementById("new_chat_empty");
const newChatArea = document.getElementById("new_chat_area");
const newChatAreas = document.getElementById("new_chat_areas");
const newChatResult = document.getElementById("new_chat_result");
const newChatCreate = document.getElementById("new_chat_create");

const PAGE_SIZE = 200;
const messageNodes = new Map(); // message id -> <li>
const rowNodes = new Map(); // chat id -> list <button>
const activityNodes = new Map(); // participant/job -> activity <li>
const askState = new Map(); // chat id -> Set of names asked to answer
let messages = []; // the open chat, ascending by seq
let hasOlder = false;
let loadTicket = 0;
let sending = false;
let newChatOpen = false;
let pendingPeer = "";
let readsNode = "";
let reads = {};
let notificationNode = "";
let notificationIDs = null;

if (composer) composer.placeholder = t("inbox.body.placeholder");
chatMembers.setAttribute("aria-label", t("inbox.participants.label"));
activityDock.setAttribute("aria-label", t("inbox.activity.label"));

function self() { return store.get().status?.node || ""; }
function chatPath(id) { return "chats/" + encodeURIComponent(id); }
function setClass(el, name, on) { el.classList.toggle(name, Boolean(on)); }
function inboxVisible() { return typeof routeFromPath !== "function" || routeFromPath(location.pathname) === "inbox"; }

function others(info) { return (info?.participants || []).filter((name) => name !== self()); }
function chatName(info) {
  if (!info) return "";
  if (info.legacy && info.peer) return info.peer;
  const names = others(info);
  return names.length ? names.join(", ") : self();
}
function authorName(name) { return name === self() ? t("inbox.you") : name; }
// genitiveName is authorName after "для"/"от": "вас" for this node.
function genitiveName(name) { return name === self() ? t("inbox.you.gen") : name; }

// whoIndex gives every name one of six stable colours, so a group chat can be
// followed by colour as well as by name.
function whoIndex(name) {
  let hash = 0;
  for (const ch of String(name)) hash = (hash * 31 + ch.codePointAt(0)) >>> 0;
  return (hash % 6) + 1;
}

function sameDay(a, b) { return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate(); }
function clock(iso) {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  const time = at.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" });
  return sameDay(at, new Date()) ? time : at.toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit" }) + " " + time;
}
function when(iso) { return new Date(iso).toLocaleString("ru-RU"); }

// elapsed renders a duration like a CLI status line: 0:07, 3:41, 1:02:03.
function elapsed(ms) {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
  const ss = String(s).padStart(2, "0");
  return h ? h + ":" + String(m).padStart(2, "0") + ":" + ss : m + ":" + ss;
}

function preview(text, limit) {
  const chars = Array.from(String(text || "").replace(/\s+/g, " ").trim());
  return chars.slice(0, limit).join("") + (chars.length > limit ? "…" : "");
}

// --- read markers: the last seq seen per chat, kept per node ---

function readsKey(node) { return "agentlink.reads.v1:" + node; }
function loadReads(chats) {
  const node = self();
  if (!node || readsNode === node || !Array.isArray(chats)) return;
  readsNode = node;
  let saved = null;
  try { saved = localStorage.getItem(readsKey(node)); } catch (_) { /* storage may be unavailable */ }
  try { reads = saved === null ? null : JSON.parse(saved); } catch (_) { reads = {}; }
  if (!reads || typeof reads !== "object") {
    // First visit: the history so far counts as read.
    reads = {};
    for (const chat of chats || []) reads[chat.id] = chat.last_seq || 0;
    persistReads();
  }
}
function persistReads() {
  if (!readsNode) return;
  try { localStorage.setItem(readsKey(readsNode), JSON.stringify(reads)); } catch (_) { /* storage may be unavailable */ }
}
function markRead(info) {
  if (!info || (reads[info.id] || 0) >= (info.last_seq || 0)) return;
  reads[info.id] = info.last_seq || 0;
  persistReads();
}
function unread(chat) {
  return Boolean(chat.last_message && chat.last_message.direction === "in" && chat.id !== store.get().selectedChat &&
    (chat.last_seq || 0) > (reads[chat.id] || 0));
}

// --- the chat list ---

function badge(chat) {
  const el = document.createElement("span");
  const kind = chat.legacy ? "legacy" : chat.closed ? "closed" : "open";
  el.className = "chat-badge " + kind;
  el.textContent = t("inbox.badge." + kind);
  return el;
}

function workingNames(chat) {
  return (chat.members || []).filter((m) => (m.jobs || []).length).map((m) => authorName(m.name));
}

function patchRow(button, chat) {
  const selected = chat.id === store.get().selectedChat && !newChatOpen;
  button.className = "conversation-choice" + (selected ? " active" : "") + (chat.active ? " live" : "");
  button.setAttribute("aria-pressed", selected ? "true" : "false");
  const top = document.createElement("span");
  top.className = "row-top";
  const who = document.createElement("strong");
  who.className = "row-who";
  who.textContent = chatName(chat);
  const at = document.createElement("span");
  at.className = "row-time";
  at.textContent = chat.last_at ? clock(chat.last_at) : "";
  top.append(who, badge(chat), at);
  const last = document.createElement("span");
  last.className = "conversation-preview";
  const lm = chat.last_message;
  last.textContent = lm ? authorName(lm.from) + ": " + preview(lm.body, 90) : (chat.title || "");
  const foot = document.createElement("span");
  foot.className = "conversation-foot";
  const live = document.createElement("span");
  live.className = "row-live";
  const working = workingNames(chat);
  live.hidden = !working.length;
  live.textContent = working.length ? fmt("inbox.working", { names: working.join(", ") }) : "";
  const fresh = document.createElement("span");
  fresh.className = "conversation-unread";
  fresh.hidden = !unread(chat);
  fresh.textContent = fresh.hidden ? "" : t("inbox.unread");
  foot.append(live, fresh);
  button.replaceChildren(top, last, foot);
}

function renderConversationList() {
  const archive = store.get().showArchive;
  const chats = (archive ? store.get().chatArchive : store.get().chats) || [];
  loadReads(store.get().chats);
  archiveToggle.textContent = t(archive ? "inbox.archive.hide" : "inbox.archive.show");
  archiveToggle.setAttribute("aria-pressed", archive ? "true" : "false");
  document.getElementById("chat_list_title").textContent = t(archive ? "inbox.archive.title" : "inbox.list.label");
  const seen = new Set();
  const rows = chats.map((chat) => {
    seen.add(chat.id);
    let button = rowNodes.get(chat.id);
    if (!button) {
      button = document.createElement("button");
      button.type = "button";
      button.dataset.chat = chat.id;
      button.addEventListener("click", () => navigate("inbox", { chat: button.dataset.chat }));
      rowNodes.set(chat.id, button);
    }
    patchRow(button, chat);
    let row = button.parentElement;
    if (!row) { row = document.createElement("li"); row.append(button); }
    return row;
  });
  for (const id of [...rowNodes.keys()]) if (!seen.has(id)) rowNodes.delete(id);
  if (!rows.length && (archive ? store.get().chatArchive : store.get().chats)) {
    const empty = document.createElement("li");
    empty.className = "empty";
    empty.textContent = t(archive ? "inbox.archive.empty" : "inbox.list.empty");
    rows.push(empty);
  }
  for (let i = 0; i < rows.length; i++) {
    if (conversationList.children[i] !== rows[i]) conversationList.insertBefore(rows[i], conversationList.children[i] || null);
  }
  while (conversationList.children.length > rows.length) conversationList.lastElementChild.remove();
}

// --- the open chat: header ---

function renderHeader(info) {
  chatTitle.textContent = info ? chatName(info) : t("inbox.select");
  const sub = [];
  if (info?.title) sub.push(info.title);
  if (info?.area) sub.push(fmt("inbox.area", { area: info.area }));
  chatSubtitle.textContent = sub.join(" — ");
  chatSubtitle.hidden = !sub.length;
  const chips = (info?.members || []).map((member) => {
    const chip = document.createElement("li");
    const state = member.self || member.connected ? (member.compatible ? "on" : "old") : "away";
    chip.className = "member-chip " + state;
    chip.style.setProperty("--who", "var(--who-" + whoIndex(member.name) + ")");
    const name = document.createElement("span");
    name.textContent = member.self ? member.name + " (" + t("inbox.you") + ")" : member.name;
    chip.append(name);
    const notes = [];
    if (!member.self) notes.push(t(state === "old" ? "inbox.member.old" : state === "on" ? "inbox.member.online" : "inbox.member.away"));
    if (member.queued) {
      const queued = document.createElement("span");
      queued.className = "member-queued";
      queued.textContent = fmt("inbox.member.queued", { n: member.queued });
      chip.append(queued);
    }
    chip.title = notes.join(", ");
    return chip;
  });
  chatMembers.replaceChildren(...chips);
  chatMembers.hidden = !chips.length;
  // Closing is the only way into the archive (a legacy chat's peer archives it too).
  chatCloseButton.hidden = !info || info.closed || info.archived;
}

// --- the open chat: timeline ---

function messageVersion(m) {
  return JSON.stringify([m.seq, m.kind, m.body, m.from, m.created_at, m.reply_to, m.responders, m.held, m.job_status,
    m.delivery, self(), store.get().chat?.closed, store.get().chat?.legacy]);
}

function createBubble() {
  const node = document.createElement("li");
  const head = document.createElement("div");
  head.className = "msg-head";
  const author = document.createElement("strong");
  author.className = "msg-author";
  const at = document.createElement("time");
  at.className = "msg-time";
  head.append(author, at);
  const quote = document.createElement("button");
  quote.type = "button";
  quote.className = "msg-quote";
  quote.addEventListener("click", () => revealMessage(node._message.reply_to));
  const body = document.createElement("pre");
  body.className = "msg-body";
  const meta = document.createElement("p");
  meta.className = "msg-meta";
  const answer = document.createElement("button");
  answer.type = "button";
  answer.className = "msg-reply";
  answer.textContent = t("inbox.reply");
  answer.addEventListener("click", () => setReply(node._message));
  node.append(head, quote, body, meta, answer);
  node._parts = { author, at, quote, body, meta, answer };
  return node;
}

function patchBubble(node, m) {
  const version = messageVersion(m);
  if (node._version === version) return;
  node._version = version;
  node._message = m;
  node.dataset.messageId = m.id;
  node.tabIndex = -1;
  const info = store.get().chat;
  const p = node._parts;
  if (m.kind === "chat_open" || m.kind === "chat_close") {
    node.className = "msg-event";
    p.author.textContent = fmt(m.kind === "chat_open" ? "inbox.event.open" : "inbox.event.close", { name: authorName(m.from) });
    p.at.textContent = clock(m.created_at);
    p.at.dateTime = m.created_at;
    p.quote.hidden = true; p.body.hidden = true; p.meta.hidden = true; p.answer.hidden = true;
    return;
  }
  // Only a failed reply is marked, never the question: another reply may answer it.
  const failed = m.job_status === "failed" && !!m.reply_to;
  node.className = "msg " + (m.direction === "out" ? "out" : "in") + (failed ? " failed-reply" : "");
  node.style.setProperty("--who", "var(--who-" + whoIndex(m.from) + ")");
  p.author.textContent = authorName(m.from);
  p.at.textContent = clock(m.created_at);
  p.at.dateTime = m.created_at;
  p.at.title = when(m.created_at);
  const parent = m.reply_to ? messages.find((item) => item.id === m.reply_to) : null;
  p.quote.hidden = !parent;
  p.quote.textContent = parent ? fmt("inbox.reply_to", { name: genitiveName(parent.from), text: preview(parent.body, 70) }) : "";
  p.body.hidden = false;
  p.body.textContent = m.body || "";
  const notes = [];
  if ((m.responders || []).length) notes.push(fmt("inbox.asks", { names: m.responders.map(genitiveName).join(", ") }));
  const waiting = (m.delivery || []).filter((d) => d.status === "queued").map((d) => d.peer);
  if (waiting.length) notes.push(fmt("inbox.undelivered", { names: waiting.join(", ") }));
  if (m.held) notes.push(t("inbox.held"));
  if (failed) notes.push(t("inbox.failed"));
  p.meta.textContent = notes.join(" · ");
  p.meta.hidden = !notes.length;
  setClass(p.meta, "warn", m.held);
  setClass(p.meta, "failed-note", failed);
  p.answer.hidden = !info || info.legacy || info.closed;
}

function reconcile(parent, nodes) {
  for (let index = 0; index < nodes.length; index++) {
    if (parent.children[index] !== nodes[index]) parent.insertBefore(nodes[index], parent.children[index] || null);
  }
  while (parent.children.length > nodes.length) parent.lastElementChild.remove();
}

let olderButton = null;
function olderControl() {
  if (!olderButton) {
    olderButton = document.createElement("li");
    olderButton.className = "msg-older";
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = t("inbox.older");
    button.addEventListener("click", loadOlder);
    olderButton.append(button);
  }
  return olderButton;
}

// renderTimeline patches the bubbles in place. The view follows new messages
// only when it was already at the bottom; otherwise it stays where it was.
function renderTimeline(opts) {
  const options = opts || {};
  const nearBottom = list.scrollHeight - list.clientHeight - list.scrollTop <= 80;
  const oldScroll = list.scrollTop;
  const oldHeight = list.scrollHeight;
  const ids = new Set(messages.map((m) => m.id));
  for (const [id, node] of messageNodes) if (!ids.has(id)) { node.remove(); messageNodes.delete(id); }
  const nodes = messages.map((m) => {
    let node = messageNodes.get(m.id);
    if (!node) { node = createBubble(); messageNodes.set(m.id, node); }
    patchBubble(node, m);
    return node;
  });
  if (hasOlder) nodes.unshift(olderControl());
  if (!messages.length) {
    const empty = document.createElement("li");
    empty.className = "empty";
    empty.textContent = t(store.get().selectedChat ? "inbox.empty_conversation" : "inbox.select_hint");
    list.replaceChildren(empty);
    return;
  }
  reconcile(list, nodes);
  const anchor = store.get().selectedMessage;
  const target = anchor ? messageNodes.get(anchor) : null;
  if (target) {
    revealMessage(anchor);
    store.patch("selectedMessage", "");
  } else if (options.prepended) list.scrollTop = oldScroll + (list.scrollHeight - oldHeight);
  else if (options.reset || nearBottom) list.scrollTop = list.scrollHeight;
  else list.scrollTop = oldScroll;
}

function revealMessage(id) {
  const target = messageNodes.get(id);
  if (!target) return;
  target.focus({ preventScroll: true });
  target.scrollIntoView({ block: "center" });
}

// --- the open chat: live activity, one row per running or queued job ---

function jobStart(job) {
  const request = messages.find((m) => m.id === job.reply_to);
  return request?.created_at || job.activity_info?.started_at || job.updated_at;
}

function activityText(job) {
  if (job.stale) return t("inbox.activity.stale");
  if (job.job_status === "queued") return t("inbox.activity.queued");
  const info = job.activity_info;
  const text = info?.text || job.activity || "";
  const typeKey = info?.type ? "inbox.activity.type." + info.type : "";
  const verb = typeKey && t(typeKey) !== typeKey ? t(typeKey) : "";
  if (verb && text) return verb + " " + text;
  return text || verb || t("inbox.activity.working");
}

function renderActivity(info) {
  const rows = [];
  const seen = new Set();
  for (const member of info?.members || []) {
    for (const job of member.jobs || []) {
      const key = member.name + "\n" + job.reply_to;
      seen.add(key);
      let row = activityNodes.get(key);
      if (!row) {
        row = document.createElement("li");
        const spin = document.createElement("span");
        spin.className = "act-spin";
        spin.setAttribute("aria-hidden", "true");
        const who = document.createElement("strong");
        who.className = "act-who";
        const text = document.createElement("span");
        text.className = "act-text";
        const step = document.createElement("span");
        step.className = "act-step";
        const total = document.createElement("span");
        total.className = "act-time";
        row.append(spin, who, text, step, total);
        row._parts = { who, text, step, total };
        activityNodes.set(key, row);
      }
      const p = row._parts;
      row.className = "act-row " + (job.stale ? "stale" : job.job_status === "queued" ? "queued" : "running");
      row.style.setProperty("--who", "var(--who-" + whoIndex(member.name) + ")");
      p.who.textContent = authorName(member.name);
      p.text.textContent = activityText(job);
      p.text.title = p.text.textContent;
      p.total.dataset.since = jobStart(job) || "";
      const stepStart = job.activity_info?.phase === "running" ? job.activity_info.started_at : "";
      p.step.dataset.since = stepStart || "";
      p.step.hidden = !stepStart || job.stale;
      rows.push(row);
    }
  }
  for (const key of [...activityNodes.keys()]) if (!seen.has(key)) activityNodes.delete(key);
  reconcile(activityDock, rows);
  activityDock.hidden = !rows.length;
  tickActivity();
}

// tickActivity advances the elapsed timers locally, once a second; it never
// asks the app for anything.
function tickActivity() {
  const now = Date.now();
  for (const row of activityNodes.values()) {
    const { total, step } = row._parts;
    const since = Date.parse(total.dataset.since);
    total.textContent = Number.isNaN(since) ? "" : elapsed(now - since);
    const stepSince = Date.parse(step.dataset.since);
    step.textContent = step.hidden || Number.isNaN(stepSince) ? "" : fmt("inbox.activity.step", { time: elapsed(now - stepSince) });
  }
}

// --- the open chat: composer ---

// askSet is the chat's "who must answer" choice: in a chat of two the other\n// side by default, in a group nobody until the user picks.
function askSet(info) {
  if (!askState.has(info.id)) {
    const names = others(info);
    const chosen = new Set(names.length === 1 ? names : []);
    if (!self()) return chosen; // the default needs to know who is "me"
    askState.set(info.id, chosen);
  }
  return askState.get(info.id);
}

function renderAsk(info) {
  const chosen = askSet(info);
  const key = info.id + "\n" + others(info).join("\n");
  askHint.hidden = chosen.size > 0;
  if (askChoices._key === key) return;
  askChoices._key = key;
  const boxes = others(info).map((name) => {
    const label = document.createElement("label");
    label.className = "choice";
    label.style.setProperty("--who", "var(--who-" + whoIndex(name) + ")");
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = name;
    box.checked = chosen.has(name);
    box.addEventListener("change", () => {
      if (box.checked) chosen.add(name); else chosen.delete(name);
      askHint.hidden = chosen.size > 0;
    });
    const text = document.createElement("span");
    text.textContent = name;
    label.append(box, text);
    return label;
  });
  askChoices.replaceChildren(...boxes);
  askHint.hidden = chosen.size > 0;
}

// legacyPeerOld: the legacy chat's peer is connected without chats, so it
// cannot take part in closing it either.
function legacyPeerOld(info) {
  return (info.members || []).some((m) => !m.self && m.connected && !m.compatible);
}

// A legacy chat is writable too: the node continues it in a real chat with the
// peer, or with a plain message when the peer's version has no chats.
function renderComposer(info) {
  const writable = info && !info.closed;
  sendForm.hidden = !writable;
  chatNote.hidden = !info || (writable && !info.legacy);
  chatNoteAction.hidden = true;
  if (!info) return;
  if (writable) renderAsk(info);
  if (info.legacy) {
    let note = fmt(legacyPeerOld(info) ? "inbox.legacy_note_old" : "inbox.legacy_note", { name: info.peer || "" });
    if (info.archived && info.closed_by) {
      note = fmt("inbox.legacy_closed_note", { name: authorName(info.closed_by), when: info.closed_at ? when(info.closed_at) : "" }) + " " + note;
    }
    chatNoteText.textContent = note;
    return;
  }
  if (writable) return;
  chatNoteText.textContent = fmt("inbox.closed_note", { name: authorName(info.closed_by || ""), when: info.closed_at ? when(info.closed_at) : "" });
  const invite = others(info);
  if (invite.length) {
    chatNoteAction.hidden = false;
    chatNoteAction.textContent = fmt("inbox.new_with", { names: invite.join(", ") });
    chatNoteAction.onclick = () => showNewChat(invite);
  }
}

function setReply(m) {
  replyTo.value = m ? m.id : "";
  document.getElementById("replying").hidden = !m;
  document.getElementById("replying_text").textContent = m ? fmt("inbox.replying", { text: authorName(m.from) + ": " + preview(m.body, 60) }) : "";
  const info = store.get().chat;
  if (m && info && m.from !== self() && others(info).includes(m.from)) {
    askState.set(info.id, new Set([m.from]));
    askChoices._key = "";
    renderAsk(info);
  }
  if (m) composer.focus();
}

function saveDraft(id) {
  if (id) store.patch("drafts", Object.assign({}, store.get().drafts, { [id]: composer.value }));
}

async function submitMessage() {
  const info = store.get().chat;
  if (sending || !info) return;
  const id = info.id;
  sending = true;
  sendButton.disabled = true;
  sendForm.setAttribute("aria-busy", "true");
  try {
    const body = { chat_id: id, body: composer.value, ask: [...askSet(info)].sort() };
    if (replyTo.value) body.reply_to = replyTo.value;
    const sent = await api("POST", "send", body);
    store.patch("drafts", Object.assign({}, store.get().drafts, { [id]: "" }));
    if (store.get().selectedChat === id) { composer.value = ""; setReply(null); }
    sendResult.textContent = t("inbox.sent");
    // A legacy chat continues elsewhere: in a real chat, or in the plain
    // message's own legacy chat.
    const next = info.legacy ? sent.chat_id || "legacy-" + sent.id + "-" + info.peer : id;
    if (next !== id) {
      refreshSlice("chats");
      if (store.get().selectedChat === id) navigate("inbox", { chat: next });
      return;
    }
    await loadChat(id, false);
  } catch (error) { sendResult.textContent = error.message; }
  finally { sending = false; sendButton.disabled = false; sendForm.removeAttribute("aria-busy"); }
}

// --- loading ---

function renderPanel() {
  const info = newChatOpen ? null : store.get().chat;
  setClass(layout, "has-chat", newChatOpen || store.get().selectedChat);
  newChatForm.hidden = !newChatOpen;
  list.hidden = newChatOpen;
  setClass(layout, "new-open", newChatOpen);
  renderHeader(info);
  if (newChatOpen) {
    chatTitle.textContent = t("inbox.new.title");
    sendForm.hidden = true; chatNote.hidden = true; activityDock.hidden = true;
    return;
  }
  renderComposer(info);
  renderActivity(info);
}

async function loadChat(id, reset) {
  const ticket = ++loadTicket;
  const first = messages[0]?.seq || 0;
  const query = !reset && first ? "?after=" + (first - 1) + "&limit=1000" : "?limit=" + PAGE_SIZE;
  const [info, items] = await Promise.all([api("GET", chatPath(id)), api("GET", chatPath(id) + "/messages" + query)]);
  if (ticket !== loadTicket || store.get().selectedChat !== id) return;
  if (reset) hasOlder = Array.isArray(items) && items.length === PAGE_SIZE;
  messages = Array.isArray(items) ? [...items].sort((a, b) => a.seq - b.seq) : [];
  store.patch("chat", info);
  markRead(info);
  if (!newChatOpen) renderPanel();
  renderTimeline({ reset });
  renderConversationList();
}

async function loadOlder() {
  const id = store.get().selectedChat;
  const first = messages[0]?.seq || 0;
  if (!id || !first) return;
  try {
    const items = await api("GET", chatPath(id) + "/messages?before=" + first + "&limit=" + PAGE_SIZE);
    if (store.get().selectedChat !== id || !Array.isArray(items)) return;
    hasOlder = items.length === PAGE_SIZE;
    const known = new Set(messages.map((m) => m.id));
    messages = items.filter((m) => !known.has(m.id)).concat(messages).sort((a, b) => a.seq - b.seq);
    renderTimeline({ prepended: true });
  } catch (error) { sendResult.textContent = error.message; }
}

async function selectChat(id, messageID) {
  const previous = store.get().selectedChat;
  if (previous !== id) {
    saveDraft(previous);
    messages = [];
    hasOlder = false;
    for (const node of messageNodes.values()) node.remove();
    messageNodes.clear();
    activityNodes.clear();
    store.patch("chat", null);
    composer.value = store.get().drafts[id] || "";
    setReply(null);
    sendResult.textContent = "";
  }
  newChatOpen = false;
  store.patch("selectedChat", id || "");
  store.patch("selectedMessage", messageID || "");
  renderPanel();
  renderConversationList();
  if (!id) { renderTimeline({}); return; }
  if (previous !== id) renderTimeline({});
  try { await loadChat(id, previous !== id || !messages.length); }
  catch (error) { sendResult.textContent = error.message; chatSubtitle.hidden = false; chatSubtitle.textContent = error.message; }
}

// chatWith finds the newest open chat of exactly this node and peer.
function chatWith(peer) {
  const want = [self(), peer].sort().join("\n");
  return (store.get().chats || []).find((chat) => !chat.legacy && !chat.closed && [...chat.participants].sort().join("\n") === want);
}

function openInbox(query) {
  const q = query || {};
  if (typeof q.chat === "string" && q.chat) { selectChat(q.chat, typeof q.message === "string" ? q.message : ""); return; }
  if (typeof q.peer === "string" && q.peer) {
    pendingPeer = q.peer;
    resolvePendingPeer();
    return;
  }
  selectChat(store.get().selectedChat, "");
}

function resolvePendingPeer() {
  if (!pendingPeer || !self() || !store.get().chats) return;
  const peer = pendingPeer;
  pendingPeer = "";
  const found = chatWith(peer);
  if (found) navigate("inbox", { chat: found.id });
  else showNewChat([peer]);
}

// --- starting a chat ---

function showNewChat(preselect) {
  saveDraft(store.get().selectedChat);
  newChatOpen = true;
  const chosen = new Set(preselect || []);
  const people = (store.get().status?.members || []).filter((m) => !m.self);
  for (const name of chosen) if (!people.some((m) => m.name === name)) people.push({ name, online: false });
  const boxes = people.map((member) => {
    const label = document.createElement("label");
    label.className = "choice" + (member.online ? " on" : "");
    label.style.setProperty("--who", "var(--who-" + whoIndex(member.name) + ")");
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = member.name;
    box.checked = chosen.has(member.name);
    const text = document.createElement("span");
    text.textContent = member.name;
    label.append(box, text);
    return label;
  });
  newChatMembers.replaceChildren(...boxes);
  newChatEmpty.hidden = boxes.length > 0;
  newChatCreate.disabled = !boxes.length;
  const settings = store.get().settings || {};
  const areas = [...new Set([...(settings.areas || []), ...Object.keys(settings.projects || {})])];
  newChatAreas.replaceChildren(...areas.map((area) => { const o = document.createElement("option"); o.value = area; return o; }));
  newChatArea.value = "";
  newChatResult.textContent = "";
  renderPanel();
  renderConversationList();
  (boxes[0]?.children[0] || newChatArea).focus();
}

function hideNewChat() {
  newChatOpen = false;
  renderPanel();
  renderConversationList();
}

async function createChat() {
  const participants = [];
  for (const label of Array.from(newChatMembers.children)) if (label.children[0].checked) participants.push(label.children[0].value);
  newChatCreate.disabled = true;
  newChatForm.setAttribute("aria-busy", "true");
  try {
    const body = { participants };
    if (newChatArea.value.trim()) body.area = newChatArea.value.trim();
    const info = await api("POST", "chats", body);
    newChatOpen = false;
    refreshSlice("chats");
    navigate("inbox", { chat: info.id });
    composer.focus();
  } catch (error) { newChatResult.textContent = error.message; }
  finally { newChatCreate.disabled = false; newChatForm.removeAttribute("aria-busy"); }
}

// --- chat actions ---

async function chatAction(path, body) {
  const info = store.get().chat;
  if (!info) return;
  try {
    const next = await api("POST", chatPath(info.id) + "/" + path, body);
    if (store.get().selectedChat === info.id) { store.patch("chat", next); renderPanel(); renderTimeline({}); }
    refreshSlice("chats");
  } catch (error) { sendResult.textContent = error.message; chatSubtitle.hidden = false; chatSubtitle.textContent = error.message; }
}

// --- notifications: a toast for a new incoming message in any chat ---

// A toast only announces news: it leaves on its own after
// MESSAGE_TOAST_TIMEOUT, and the close button drops it right away. Failures
// keep their own place — the connection banner in common.js stays until the
// problem is gone.
const MESSAGE_TOAST_TIMEOUT = 6000;

function notificationKey(node) { return "agentlink.notifications.v1:" + node; }
function incomingNotifications(chats) {
  return (chats || []).filter((chat) => chat.last_message && chat.last_message.direction === "in")
    .map((chat) => ({ notificationID: chat.last_message.id, chat: chat.id, id: chat.last_message.id, from: chat.last_message.from, body: chat.last_message.body }));
}
function persistNotificationIDs(node, ids) {
  const bounded = [...new Set(ids)].slice(0, 500);
  try { localStorage.setItem(notificationKey(node), JSON.stringify(bounded)); } catch (_) { /* storage may be unavailable */ }
  notificationIDs = bounded;
}

function showMessageToast(item) {
  const region = document.getElementById("message-toast-region");
  if (!region) return;
  while (region.childElementCount >= 3 && region.firstElementChild) region.firstElementChild.remove();
  const toast = document.createElement("div");
  toast.className = "message-toast";
  const open = document.createElement("button");
  open.type = "button";
  open.className = "message-toast-main";
  const sender = document.createElement("strong");
  sender.textContent = item.from;
  const text = document.createElement("span");
  text.className = "message-toast-preview";
  text.textContent = preview(item.body, 120);
  open.append(sender, text);
  const close = document.createElement("button");
  close.type = "button";
  close.className = "message-toast-close";
  close.textContent = "×";
  close.setAttribute("aria-label", t("inbox.toast.close"));
  const timer = setTimeout(() => toast.remove(), MESSAGE_TOAST_TIMEOUT);
  const dismiss = () => { clearTimeout(timer); toast.remove(); };
  close.addEventListener("click", dismiss);
  open.addEventListener("click", () => {
    dismiss();
    navigate("inbox", { chat: item.chat, message: item.id });
  });
  toast.append(open, close);
  region.append(toast);
}

function processIncomingChats(chats) {
  const node = self();
  if (!node || !Array.isArray(chats)) return;
  const items = incomingNotifications(chats);
  if (notificationNode !== node) {
    notificationNode = node;
    let saved = null;
    try { saved = localStorage.getItem(notificationKey(node)); } catch (_) { /* storage may be unavailable */ }
    if (saved === null) { persistNotificationIDs(node, items.map((item) => item.notificationID)); return; }
    try { notificationIDs = JSON.parse(saved); } catch (_) { notificationIDs = []; }
    if (!Array.isArray(notificationIDs)) notificationIDs = [];
  }
  const known = new Set(notificationIDs);
  const open = inboxVisible() ? store.get().selectedChat : "";
  const unseen = items.filter((item) => !known.has(item.notificationID) && item.chat !== open);
  persistNotificationIDs(node, items.map((item) => item.notificationID).concat(notificationIDs));
  for (const item of unseen.slice(0, 3)) showMessageToast(item);
}

// --- wiring ---

function onChats() {
  processIncomingChats(store.get().chats);
  renderConversationList();
  resolvePendingPeer();
  const id = store.get().selectedChat;
  if (id && inboxVisible()) loadChat(id, !messages.length).catch((error) => { sendResult.textContent = error.message; });
}

document.getElementById("cancel_reply").addEventListener("click", () => setReply(null));
composer.addEventListener("input", () => saveDraft(store.get().selectedChat));
composer.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) { event.preventDefault(); submitMessage(); }
});
sendForm.addEventListener("submit", (event) => { event.preventDefault(); submitMessage(); });
newChatButton.addEventListener("click", () => showNewChat([]));
newChatForm.addEventListener("submit", (event) => { event.preventDefault(); createChat(); });
document.getElementById("new_chat_dismiss").addEventListener("click", hideNewChat);
archiveToggle.addEventListener("click", () => {
  store.patch("showArchive", !store.get().showArchive);
  renderConversationList();
  if (store.get().showArchive) refreshSlice("chats");
});
chatBack.addEventListener("click", () => {
  if (newChatOpen) { hideNewChat(); if (!store.get().selectedChat) return; }
  selectChat("", "");
  navigate("inbox");
});
chatCloseButton.addEventListener("click", () => {
  const info = store.get().chat;
  const key = !info?.legacy ? "inbox.close.confirm" : legacyPeerOld(info) ? "inbox.close.confirm_old" : "inbox.close.confirm_legacy";
  if (typeof confirm === "function" && !confirm(t(key))) return;
  chatAction("close");
});
store.subscribe("chats", onChats);
store.subscribe("chatArchive", renderConversationList);
store.subscribe("status", () => {
  renderConversationList();
  if (store.get().chat && !newChatOpen) renderPanel();
  processIncomingChats(store.get().chats);
  resolvePendingPeer();
});
setInterval(tickActivity, 1000);
renderConversationList();
