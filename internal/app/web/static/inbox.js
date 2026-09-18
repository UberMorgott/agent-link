"use strict";

const list = document.getElementById("messages");
const sendForm = document.getElementById("send");
const sendResult = document.getElementById("result");
const replyTo = document.getElementById("reply_to");

function setReply(id, from) {
  replyTo.value = id || "";
  document.getElementById("replying").hidden = !id;
  document.getElementById("replying_text").textContent = id ? fmt("inbox.replying", { id: id.slice(0, 8) }) : "";
  if (from) document.getElementById("to").value = from;
  if (id) document.getElementById("body").focus();
}

function when(iso) {
  return new Date(iso).toLocaleString();
}

function statusText(status) {
  return fmt("inbox.status", { status: t("status." + status) });
}

// block renders one labelled piece of text ("Вопрос", "Ответ").
function block(labelKey, text, cls) {
  const wrap = document.createElement("div");
  wrap.className = cls;
  const label = document.createElement("span");
  label.className = "tag";
  label.textContent = t(labelKey);
  const body = document.createElement("pre");
  body.textContent = text;
  wrap.append(label, body);
  return wrap;
}

// render draws one row per question, with its answer folded into the same row.
function render(items) {
  list.replaceChildren();
  if (!items.length) {
    const empty = document.createElement("li");
    empty.className = "empty";
    empty.textContent = t("inbox.empty");
    list.append(empty);
    return;
  }
  for (const th of items) {
    const li = document.createElement("li");
    li.className = th.direction;
    const head = document.createElement("div");
    head.className = "head";
    const dir = document.createElement("strong");
    dir.textContent = t(th.direction === "in" ? "inbox.dir.in" : "inbox.dir.out");
    const parts = [fmt("inbox.route", { from: th.from, to: th.to })];
    if (th.area) parts.push(fmt("inbox.area", { area: th.area }));
    parts.push(when(th.created_at), statusText(th.status));
    head.append(dir, document.createTextNode(" · " + parts.join(" · ")));
    li.append(head, block("inbox.question", th.body, "question"));
    if (th.answered) {
      li.append(block("inbox.answer", th.answer, "answer"));
      if (th.answer_at) {
        const at = document.createElement("div");
        at.className = "head";
        at.textContent = when(th.answer_at);
        li.append(at);
      }
    } else {
      const none = document.createElement("p");
      none.className = "hint";
      none.textContent = t("inbox.no_answer");
      li.append(none);
      if (th.activity) {
        const now = document.createElement("p");
        now.className = "activity";
        now.textContent = fmt("inbox.activity", { activity: th.activity });
        li.append(now);
      }
      if (th.no_news_min) {
        const silent = document.createElement("p");
        silent.className = "warn";
        silent.textContent = fmt("inbox.no_news", { min: String(th.no_news_min) });
        li.append(silent);
      }
    }
    if (th.replyable) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.textContent = t("inbox.reply");
      btn.addEventListener("click", () => setReply(th.id, th.from));
      li.append(btn);
    }
    list.append(li);
  }
}

async function refresh() {
  try {
    render(await api("GET", "threads"));
  } catch (e) {
    sendResult.textContent = e.message;
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
    sendResult.textContent = t("inbox.sent");
    refresh();
  } catch (e) {
    sendResult.textContent = e.message;
  }
});

api("GET", "status").then((s) => {
  const to = document.getElementById("to");
  if (!to.value) to.value = s.peer || "";
}).catch(() => {});

refresh();
setInterval(refresh, 3000);
