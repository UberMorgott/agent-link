"use strict";

// The inbox: a list of chats on the left, the open chat on the right. Chats,
// their messages and the live activity of every participant come from
// /ui/api/chats; SSE "chats"/"messages"/"sessions" events reload them
// (common.js). The open chat is one centered column: messages, one activity
// line per working agent, the composer. Technical detail (connection, queues,
// sessions) sits behind the header's info disclosure.

const layout = document.getElementById("conversation_layout");
const conversationList = document.getElementById("conversation_list");
const archiveToggle = document.getElementById("archive_toggle");
const archiveToggleText = document.getElementById("archive_toggle_text");
const newChatButton = document.getElementById("new_chat");
const list = document.getElementById("messages");
const chatTitle = document.getElementById("conversation_title");
const chatSubtitle = document.getElementById("chat_subtitle");
const chatInfo = document.getElementById("chat_info");
const chatMembers = document.getElementById("chat_members");
const chatSessions = document.getElementById("chat_sessions");
const chatCloseButton = document.getElementById("chat_close");
const chatBack = document.getElementById("chat_back");
const activityDock = document.getElementById("chat_activity");
const sendForm = document.getElementById("send");
const sendButton = document.getElementById("send_button");
const sendResult = document.getElementById("inbox_result");
const askRow = document.getElementById("ask_row");
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
// A message within GROUP_MS of the previous one by the same author continues it
// without repeating the author line.
const GROUP_MS = 5 * 60 * 1000;
const messageNodes = new Map(); // message id -> <li>
const rowNodes = new Map(); // chat id -> list <button>
const activityNodes = new Map(); // participant/job -> activity <li>
const askState = new Map(); // chat id -> Set of names asked to answer
let messages = []; // the open chat, ascending by seq
let hasOlder = false;
let loadTicket = 0;
let sending = false;
let closing = false;
let newChatOpen = false;
let pendingPeer = "";
let readsNode = "";
let reads = {};
let notificationNode = "";
let notificationIDs = null;

// Static inline icons (no user data inside), drawn with currentColor.
const ICONS = {
  queued: '<svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="5.6"/><path d="M8 5v3.2l2 1.3"/></svg>',
  delivered: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.4l2.9 2.9 6.1-6.6"/></svg>',
  read: '<svg viewBox="0 0 20 16" aria-hidden="true"><path d="M1.8 8.4l2.9 2.9 6.1-6.6M8.6 10.6l.7.7 6.1-6.6"/></svg>',
  held: '<svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="5.6"/><path d="M8 5v3.6M8 10.8v.1"/></svg>',
  agent: '<svg viewBox="0 0 16 16" aria-hidden="true"><rect x="3" y="5" width="10" height="8" rx="2.2"/><path d="M8 2.5V5M6 9h.01M10 9h.01"/></svg>',
  human: '<svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="5.6" r="2.6"/><path d="M3.2 13.5c.6-2.4 2.5-3.8 4.8-3.8s4.2 1.4 4.8 3.8"/></svg>',
};

if (composer) composer.placeholder = t("inbox.body.placeholder");
chatMembers.setAttribute("aria-label", t("inbox.participants.label"));
activityDock.setAttribute("aria-label", t("inbox.activity.label"));
chatInfo.title = t("inbox.info");
chatCloseButton.title = t("inbox.close");

function self() { return store.get().status?.node || ""; }
function chatPath(id) { return "chats/" + encodeURIComponent(id); }
function setClass(el, name, on) { el.classList.toggle(name, Boolean(on)); }
function inboxVisible() { return typeof routeFromPath !== "function" || routeFromPath(location.pathname) === "inbox"; }
function narrow() { return typeof matchMedia === "function" && matchMedia("(max-width: 700px)").matches; }

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

// A person and their agent write from the same node; author_kind tells them
// apart. A message of an old peer has no kind and counts as the person's.
function isAgent(m) { return m.author_kind === "agent" || m.author_kind === "worker"; }
function agentName(name) { return name === self() ? t("inbox.author.own_agent") : fmt("inbox.author.agent", { name }); }
function authorLabel(m) { return isAgent(m) ? agentName(m.from) : authorName(m.from); }

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

// --- live activity text, shared by the chat list and the open chat ---

function activityText(job) {
  if (job.stale) return t("inbox.activity.stale");
  if (job.job_status === "queued") return t("inbox.activity.queued");
  const info = job.activity_info;
  const text = info?.text || job.activity || "";
  const typeKey = info?.type ? "inbox.activity.type." + info.type : "";
  const verb = typeKey && t(typeKey) !== typeKey ? t(typeKey) : "";
  // A step that only names its type ("thinking") reads as the verb alone.
  if (verb && text && text.toLowerCase() !== info.type.toLowerCase()) return verb + " " + text;
  return verb || text || t("inbox.activity.working");
}

function workingLines(chat) {
  const out = [];
  for (const m of chat.members || []) for (const job of m.jobs || []) out.push(agentName(m.name) + " " + activityText(job));
  return out;
}

// --- the chat list ---

function patchRow(button, chat) {
  const selected = chat.id === store.get().selectedChat && !newChatOpen;
  const working = workingLines(chat);
  const fresh = unread(chat);
  button.className = "conversation-choice" + (selected ? " active" : "") + (working.length ? " live" : "") + (fresh ? " fresh" : "");
  button.setAttribute("aria-pressed", selected ? "true" : "false");
  const top = document.createElement("span");
  top.className = "row-top";
  const who = document.createElement("strong");
  who.className = "row-who";
  who.textContent = chatName(chat);
  top.append(who);
  if (chat.legacy) {
    const badge = document.createElement("span");
    badge.className = "chat-badge legacy";
    badge.textContent = t("inbox.badge.legacy");
    top.append(badge);
  }
  const at = document.createElement("span");
  at.className = "row-time";
  at.textContent = chat.last_at ? clock(chat.last_at) : "";
  top.append(at);
  const foot = document.createElement("span");
  foot.className = "row-foot";
  const last = document.createElement("span");
  const lm = chat.last_message;
  if (working.length) {
    last.className = "row-live";
    last.textContent = working[0] + (working.length > 1 ? " +" + (working.length - 1) : "");
  } else {
    last.className = "conversation-preview";
    last.textContent = lm ? authorLabel(lm) + ": " + preview(lm.body, 90) : (chat.title || "");
  }
  const dot = document.createElement("span");
  dot.className = "conversation-unread";
  dot.hidden = !fresh;
  dot.textContent = fresh ? t("inbox.unread") : "";
  foot.append(last, dot);
  button.replaceChildren(top, foot);
}

function renderConversationList() {
  const archive = store.get().showArchive;
  const chats = (archive ? store.get().chatArchive : store.get().chats) || [];
  loadReads(store.get().chats);
  archiveToggleText.textContent = t(archive ? "inbox.archive.hide" : "inbox.archive.show");
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
  reconcile(conversationList, rows);
}

// --- the open chat: header and its info disclosure ---

function memberState(member) { return member.self || member.connected ? (member.compatible ? "on" : "old") : "away"; }

function renderHeader(info) {
  chatTitle.textContent = info ? chatName(info) : t("inbox.select");
  // The subtitle: who is reachable right now, and the chat's project area.
  const parts = [];
  for (const member of info?.members || []) {
    if (member.self) continue;
    const state = memberState(member);
    const item = document.createElement("span");
    item.className = "presence " + state;
    item.textContent = member.name + " — " + t(state === "old" ? "inbox.member.old_short" : state === "on" ? "inbox.member.online" : "inbox.member.away");
    parts.push(item);
  }
  if (info?.area) {
    const area = document.createElement("span");
    area.className = "chat-area";
    area.textContent = fmt("inbox.area", { area: info.area });
    parts.push(area);
  }
  chatSubtitle.replaceChildren(...parts);
  chatSubtitle.hidden = !parts.length;
  const chips = (info?.members || []).map((member) => {
    const chip = document.createElement("li");
    const state = memberState(member);
    chip.className = "member-chip " + state;
    chip.style.setProperty("--who", "var(--who-" + whoIndex(member.name) + ")");
    const name = document.createElement("strong");
    name.textContent = member.self ? member.name + " (" + t("inbox.you") + ")" : member.name;
    chip.append(name);
    const notes = [];
    if (!member.self) notes.push(t(state === "old" ? "inbox.member.old" : state === "on" ? "inbox.member.online" : "inbox.member.away"));
    if (member.queued) notes.push(fmt("inbox.member.queued", { n: member.queued }));
    for (const job of member.held || []) notes.push(job.activity || t("inbox.hold.unknown"));
    if (notes.length) {
      const detail = document.createElement("span");
      detail.className = "member-note";
      detail.textContent = notes.join(" · ");
      chip.append(detail);
    }
    return chip;
  });
  chatMembers.replaceChildren(...chips);
  renderSessions(info);
  chatInfo.hidden = !info;
  // Closing is the only way into the archive (a legacy chat's peer archives it too).
  chatCloseButton.hidden = !info || info.closed || info.archived;
}

// localArea is the session area a chat's messages reach on this computer: an
// area without a project folder here goes to the working folder ("").
function localArea(area) {
  const projects = store.get().settings?.projects || {};
  return area && Object.prototype.hasOwnProperty.call(projects, area) ? area : "";
}
function chatSessionList(info) {
  const area = localArea(info?.area || "");
  return (Array.isArray(store.get().sessions) ? store.get().sessions : []).filter((s) => (s.area || "") === area);
}

function renderSessions(info) {
  const items = chatSessionList(info).map((s) => {
    const li = document.createElement("li");
    li.textContent = [s.provider || "", s.folder || "", t(s.wake === "rewake" ? "inbox.session.rewake" : "inbox.session.next_event")].filter(Boolean).join(" · ");
    return li;
  });
  if (!items.length) {
    const li = document.createElement("li");
    li.className = "empty";
    li.textContent = t("inbox.info.no_sessions");
    items.push(li);
  }
  chatSessions.replaceChildren(...items);
}

// --- the open chat: delivery ticks ---

// The recipient's progress; answered reads as read (the reply itself shows).
const TICK_RANK = { queued: 0, delivered: 1, read: 2 };
function tickState(d) {
  if (d.state === "answered" || d.state === "read") return "read";
  if (d.state === "delivered" || d.state === "queued") return d.state;
  return d.status === "sent" ? "delivered" : "queued";
}

// ticksFor is the tick of an own message: the lowest state across its
// recipients, or a hold «!» with the reason. Null when there is nothing to show.
function ticksFor(m) {
  if (m.direction !== "out" || m.kind) return null;
  const holds = holdsFor(m.id);
  const failed = m.job_status === "failed" && !!m.reply_to;
  if (m.held || holds.length || failed) {
    const lines = holds.map((h) => h.name + ": " + h.text);
    if (m.held) lines.unshift(t("inbox.held"));
    if (failed) lines.unshift(t("inbox.failed"));
    return { state: "held", label: lines.join("\n") };
  }
  const delivery = m.delivery || [];
  if (!delivery.length) return null;
  let lowest = "read";
  const lines = delivery.map((d) => {
    const state = tickState(d);
    if (TICK_RANK[state] < TICK_RANK[lowest]) lowest = state;
    const word = t("inbox.tick." + (d.state === "answered" ? "answered" : state));
    return d.peer + ": " + word + (state === "read" && d.at ? " " + clock(d.at) : "");
  });
  return { state: lowest, label: delivery.length === 1 ? lines[0] : lines.join("\n") };
}

// --- the open chat: timeline ---

// holdsFor lists the participants that were asked message id but will not
// answer it automatically, with why (their node's text); such a hold is idle.
function holdsFor(id) {
  const out = [];
  for (const member of store.get().chat?.members || []) {
    for (const job of member.held || []) if (job.reply_to === id) out.push({ name: member.name, text: job.activity || t("inbox.hold.unknown") });
  }
  return out;
}

function messageVersion(m, cont) {
  return JSON.stringify([m.seq, m.kind, m.body, m.from, m.author_kind, m.own_human, m.unread, m.created_at, m.reply_to, m.responders, m.held,
    m.job_status, m.delivery, self(), store.get().chat?.closed, store.get().chat?.legacy, others(store.get().chat).length, holdsFor(m.id), cont]);
}

function createBubble() {
  const node = document.createElement("li");
  const head = document.createElement("div");
  head.className = "msg-head";
  const icon = document.createElement("span");
  icon.className = "msg-icon";
  const author = document.createElement("strong");
  author.className = "msg-author";
  const fyi = document.createElement("span");
  fyi.className = "msg-fyi";
  head.append(icon, author, fyi);
  const quote = document.createElement("button");
  quote.type = "button";
  quote.className = "msg-quote";
  quote.addEventListener("click", () => revealMessage(node._message.reply_to));
  const body = document.createElement("pre");
  body.className = "msg-body";
  const foot = document.createElement("div");
  foot.className = "msg-foot";
  const note = document.createElement("span");
  note.className = "msg-note";
  const at = document.createElement("time");
  at.className = "msg-time";
  const ticks = document.createElement("span");
  ticks.className = "msg-ticks";
  ticks.setAttribute("role", "img");
  foot.append(note, at, ticks);
  const answer = document.createElement("button");
  answer.type = "button";
  answer.className = "msg-reply";
  answer.textContent = t("inbox.reply");
  answer.addEventListener("click", () => setReply(node._message));
  node.append(head, quote, body, foot, answer);
  node._parts = { head, icon, author, fyi, at, quote, body, foot, note, ticks, answer };
  return node;
}

function patchBubble(node, m, cont) {
  const version = messageVersion(m, cont);
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
    p.icon.hidden = true; p.fyi.hidden = true;
    p.at.textContent = clock(m.created_at);
    p.at.dateTime = m.created_at;
    p.head.append(p.at);
    p.quote.hidden = true; p.body.hidden = true; p.foot.hidden = true; p.answer.hidden = true;
    return;
  }
  const agent = isAgent(m);
  const out = m.direction === "out";
  node.className = "msg " + (out ? "out" : "in") + (agent ? " agent" : " human") + (cont ? " cont" : "");
  node.style.setProperty("--who", "var(--who-" + whoIndex(m.from) + ")");
  p.icon.hidden = false;
  p.icon.innerHTML = agent ? ICONS.agent : ICONS.human;
  p.author.textContent = authorLabel(m);
  // A person's own message the local agent has not seen yet: it gets it as
  // information, not as a request.
  p.fyi.hidden = !(m.own_human && m.unread);
  p.fyi.textContent = p.fyi.hidden ? "" : t("inbox.author.fyi");
  p.at.textContent = clock(m.created_at);
  p.at.dateTime = m.created_at;
  p.at.title = when(m.created_at);
  const parent = m.reply_to ? messages.find((item) => item.id === m.reply_to) : null;
  p.quote.hidden = !parent;
  p.quote.textContent = parent ? fmt("inbox.reply_to", { name: genitiveName(parent.from), text: preview(parent.body, 70) }) : "";
  p.body.hidden = false;
  p.body.textContent = m.body || "";
  // Whom a group message asks; in a chat of two it is always the other side.
  const asks = (m.responders || []).filter((name) => name !== m.from);
  p.note.hidden = !(asks.length && others(info).length > 1);
  p.note.textContent = p.note.hidden ? "" : fmt("inbox.asks", { names: asks.map(genitiveName).join(", ") });
  const tick = out ? ticksFor(m) : (m.held ? { state: "held", label: t("inbox.held") } : null);
  p.ticks.hidden = !tick;
  p.ticks.className = "msg-ticks" + (tick ? " " + tick.state : "");
  p.ticks.innerHTML = tick ? ICONS[tick.state] : "";
  p.ticks.title = tick ? tick.label : "";
  p.ticks.setAttribute("aria-label", tick ? tick.label.replace(/\n/g, "; ") : "");
  // Own messages get no reply button: a reference to oneself asks nobody.
  p.answer.hidden = !info || info.legacy || info.closed || out;
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

// continues: m follows prev by the same author and kind, soon after.
function continues(prev, m) {
  if (!prev || prev.kind || m.kind || prev.from !== m.from || isAgent(prev) !== isAgent(m) || m.reply_to) return false;
  const gap = Date.parse(m.created_at) - Date.parse(prev.created_at);
  return gap >= 0 && gap < GROUP_MS;
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
  const nodes = messages.map((m, i) => {
    let node = messageNodes.get(m.id);
    if (!node) { node = createBubble(); messageNodes.set(m.id, node); }
    patchBubble(node, m, continues(messages[i - 1], m));
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

// --- the open chat: live activity, one line per running or queued job ---

function jobStart(job) {
  const request = messages.find((m) => m.id === job.reply_to);
  return request?.created_at || job.activity_info?.started_at || job.updated_at;
}

// waitingLine: this computer's agent has unread messages of the chat but runs
// nothing for them — say why, so a silent chat is never a mystery.
function waitingLine(info) {
  if (!info || info.closed || info.legacy) return null;
  const mine = (info.members || []).find((m) => m.self);
  if ((mine?.jobs || []).length) return null;
  const pending = messages.filter((m) => m.unread && m.direction === "in" && !m.kind);
  if (!pending.length) return null;
  const sessions = chatSessionList(info);
  let key = "";
  if (sessions.length && sessions.every((s) => s.wake !== "rewake")) key = "inbox.activity.waiting_session";
  else if (!sessions.length && !store.get().settings?.auto_answer) key = "inbox.activity.no_session";
  if (!key) return null;
  return { name: self(), text: t(key), since: pending[0].created_at };
}

// presenceLines: for every recipient of the last own message that has it but
// has not read it yet, what its node says of its session there — so the
// sender knows whether it is read at once or waits. Offline: the tick says it.
const PRESENCE_KEY = { rewake: "inbox.presence.rewake", "next-event": "inbox.presence.next_event" };
function presenceLines(info) {
  if (!info || info.closed || info.legacy) return [];
  const last = [...messages].reverse().find((m) => m.direction === "out" && !m.kind);
  if (!last) return [];
  const out = [];
  for (const d of last.delivery || []) {
    if (tickState(d) !== "delivered") continue;
    const member = (info.members || []).find((m) => !m.self && m.name === d.peer);
    const p = member?.connected ? member.presence : null;
    if (!p || (member.jobs || []).length) continue;
    const key = PRESENCE_KEY[p.session] || (p.auto_answer ? "inbox.presence.worker" : "inbox.presence.none");
    out.push({ name: d.peer, text: t(key) });
  }
  return out;
}

function activityRow(key) {
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
    const total = document.createElement("span");
    total.className = "act-time";
    row.append(spin, who, text, total);
    row._parts = { who, text, total };
    activityNodes.set(key, row);
  }
  return row;
}

function renderActivity(info) {
  const rows = [];
  const seen = new Set();
  const place = (key, cls, name, text, since) => {
    seen.add(key);
    const row = activityRow(key);
    const p = row._parts;
    row.className = "act-row " + cls;
    row.style.setProperty("--who", "var(--who-" + whoIndex(name) + ")");
    p.who.textContent = agentName(name);
    p.text.textContent = text;
    p.text.title = text;
    // One timer: how long it has taken so far.
    p.total.dataset.since = since || "";
    rows.push(row);
  };
  for (const member of info?.members || []) {
    for (const job of member.jobs || []) {
      place(member.name + "\n" + job.reply_to, job.stale ? "stale" : job.job_status === "queued" ? "queued" : "running", member.name, activityText(job), jobStart(job));
    }
  }
  const waiting = waitingLine(info);
  if (waiting) place("\nwaiting", "waiting", waiting.name, waiting.text, waiting.since);
  for (const line of presenceLines(info)) {
    place("\npresence\n" + line.name, "presence", line.name, line.text, "");
    activityNodes.get("\npresence\n" + line.name)._parts.who.textContent = line.name + ":";
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
    const { total } = row._parts;
    const since = Date.parse(total.dataset.since);
    total.textContent = Number.isNaN(since) ? "" : "· " + elapsed(now - since);
  }
}

// --- the open chat: composer ---

// askSet is the chat's "who must answer" choice: in a chat of two the other
// side by default, in a group nobody until the user picks.
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
  // A chat of two always asks the other side: the choice is only for groups.
  askRow.hidden = others(info).length < 2;
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
  document.getElementById("replying_text").textContent = m ? fmt("inbox.replying", { text: authorLabel(m) + ": " + preview(m.body, 60) }) : "";
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
  sendResult.textContent = "";
  try {
    const body = { chat_id: id, body: composer.value, ask: [...askSet(info)].sort() };
    if (replyTo.value) body.reply_to = replyTo.value;
    const sent = await api("POST", "send", body);
    store.patch("drafts", Object.assign({}, store.get().drafts, { [id]: "" }));
    if (store.get().selectedChat === id) { composer.value = ""; setReply(null); }
    // A legacy chat continues elsewhere: in a real chat, or in the plain
    // message's own legacy chat; a closed chat in its conversation's next one.
    const next = info.legacy ? sent.chat_id || "legacy-" + sent.id + "-" + info.peer : sent.chat_id || id;
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
  setClass(layout, "no-chat", !newChatOpen && !store.get().selectedChat);
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
    chatInfo.open = false;
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

// --- closing: the chat leaves the list at once and the view moves on ---

async function closeChat() {
  const info = store.get().chat;
  if (!info || closing) return;
  closing = true;
  chatCloseButton.disabled = true;
  try {
    await api("POST", chatPath(info.id) + "/close");
    const rest = (store.get().chats || []).filter((chat) => chat.id !== info.id);
    const next = narrow() ? null : rest.find((chat) => !chat.legacy) || rest[0];
    // Leave the chat before the list changes, so nothing reloads it.
    await selectChat("", "");
    store.patch("chats", rest);
    navigate("inbox", next ? { chat: next.id } : undefined);
    refreshSlice("chats");
  } catch (error) { sendResult.textContent = error.message; chatSubtitle.hidden = false; chatSubtitle.textContent = error.message; }
  finally { closing = false; chatCloseButton.disabled = false; }
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
    .map((chat) => ({ notificationID: chat.last_message.id, chat: chat.id, id: chat.last_message.id, from: authorLabel(chat.last_message), body: chat.last_message.body }));
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
  closeChat();
});
store.subscribe("chats", onChats);
store.subscribe("chatArchive", renderConversationList);
store.subscribe("sessions", () => { if (store.get().chat && !newChatOpen) renderPanel(); });
store.subscribe("status", () => {
  renderConversationList();
  if (store.get().chat && !newChatOpen) renderPanel();
  processIncomingChats(store.get().chats);
  resolvePendingPeer();
});
setInterval(tickActivity, 1000);
renderConversationList();
