package app

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"golang.org/x/net/html"
)

// --- the page dictionary ---

var (
	dataTKey   = regexp.MustCompile(`data-t="([^"]+)"`)
	titleKey   = regexp.MustCompile(`content="(page\.[^"]+)"`)
	scriptKey  = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)
	dynPrefix  = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)*\.)"\s*\+`)
	cyrillic   = regexp.MustCompile(`\p{Cyrillic}`)
	englishOut = []string{
		"Settings", "Inbox", "Save", "Send", "Reply", "Replying", "Generate", "Copy", "Show",
		"This computer", "The other person", "Answering requests", "Shared secret", "Handler agent",
		"Working folder", "My name", "Their name", "My address", "Their address", "Message",
		"not configured", "offline", "is not running", "Queued", "Not sent", "Not saved", "Saved",
		"Could not load", "None", "Cancel",
	}
)

// webFiles returns every embedded page and script by name.
func webFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".css") {
			return err
		}
		data, err := webFS.ReadFile(p)
		files[p] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestUIStringsCoverPages checks that the pages and the dictionary agree: no
// key used by a page is missing, and no dictionary entry is dead.
func TestUIStringsCoverPages(t *testing.T) {
	used := map[string]bool{}
	prefixes := map[string]bool{}
	for name, body := range webFiles(t) {
		for _, re := range []*regexp.Regexp{dataTKey, titleKey, scriptKey} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				if _, ok := uiStrings[m[1]]; !ok {
					t.Errorf("%s uses key %q, missing from uiStrings", name, m[1])
				}
				used[m[1]] = true
			}
		}
		for _, m := range dynPrefix.FindAllStringSubmatch(body, -1) {
			prefixes[m[1]] = true
		}
	}
	// The server fills some text itself: status problems and error sentences.
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range goFiles {
		if strings.HasSuffix(name, "_test.go") || name == "strings.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range scriptKey.FindAllStringSubmatch(string(data), -1) {
			used[m[1]] = true
		}
		for _, m := range dynPrefix.FindAllStringSubmatch(string(data), -1) {
			prefixes[m[1]] = true
		}
	}
	for key := range uiStrings {
		if used[key] {
			continue
		}
		covered := false
		for p := range prefixes {
			if strings.HasPrefix(key, p) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("uiStrings key %q is never used by a page", key)
		}
	}
}

// TestUIStringsAreRussian checks the values themselves: Cyrillic text, no
// leftover English label.
func TestUIStringsAreRussian(t *testing.T) {
	// Brand names and a pure "{from} → {to}" template carry no words to translate.
	brand := map[string]bool{"settings.handler.claude": true, "settings.handler.codex": true, "inbox.route": true}
	for key, val := range uiStrings {
		if val == "" {
			t.Errorf("uiStrings[%q] is empty", key)
		}
		if !brand[key] && !cyrillic.MatchString(val) {
			t.Errorf("uiStrings[%q] = %q has no Russian text", key, val)
		}
	}
}

// TestApplicationShellSeparatesViewResults keeps async feedback in the view
// that initiated it instead of writing it into another hidden route.
func TestApplicationShellSeparatesViewResults(t *testing.T) {
	files := webFiles(t)
	for _, c := range []struct{ file, id string }{
		{"web/app.html", "inbox_result"},
		{"web/app.html", "settings_result"},
		{"web/static/inbox.js", "inbox_result"},
		{"web/static/settings.js", "settings_result"},
	} {
		if !strings.Contains(files[c.file], `"`+c.id+`"`) {
			t.Errorf("%s does not use #%s", c.file, c.id)
		}
	}
}

// TestUIShellKeepsOneLiveRegionAndRealRoutes protects the persistent shell:
// route changes update its views without duplicating navigation or alerts.
func TestUIShellKeepsOneLiveRegionAndRealRoutes(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(t, http.MethodGet, "/ui/dashboard", "", nil)
	if code != http.StatusOK {
		t.Fatalf("dashboard shell: %d", code)
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	routes := map[string]string{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			var id, href, route, live string
			for _, attr := range n.Attr {
				switch attr.Key {
				case "id":
					id = attr.Val
				case "href":
					href = attr.Val
				case "data-route":
					route = attr.Val
				case "aria-live":
					live = attr.Val
				}
			}
			if id == "app-shell" {
				counts["shell"]++
			}
			if live != "" {
				counts["live"]++
			}
			if n.Data == "a" && route != "" {
				routes[route] = href
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if counts["shell"] != 1 || counts["live"] != 1 {
		t.Fatalf("shell=%d live=%d, want one each", counts["shell"], counts["live"])
	}
	for _, route := range []string{"dashboard", "inbox", "participants", "settings"} {
		if routes[route] != "/ui/"+route {
			t.Errorf("route %q href=%q, want /ui/%s", route, routes[route], route)
		}
	}
	for _, id := range []string{"dashboard_cards", "dashboard_recent"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("dashboard view is missing #%s", id)
		}
	}
}

// TestDashboardRefreshReflectsNodeChanges verifies that a stable dashboard
// route and token receive fresh node counts on the next API request.
func TestDashboardRefreshReflectsNodeChanges(t *testing.T) {
	h := newHarness(t)
	read := func() DashboardSummary {
		code, body := h.do(t, http.MethodGet, "/ui/api/dashboard", "", h.tokenHdr())
		var summary DashboardSummary
		if err := json.Unmarshal([]byte(body), &summary); err != nil || code != http.StatusOK {
			t.Fatalf("dashboard: %d %s err=%v", code, body, err)
		}
		return summary
	}
	before := read()
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/send", `{"to":"bob","body":"проверь обновление"}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	after := read()
	if before.Status.Total == after.Status.Total || before.TotalMessages == after.TotalMessages {
		t.Fatalf("dashboard did not refresh counts: before=%+v after=%+v", before, after)
	}
}

// TestReactiveStoreKeepsWarningUntilEveryCoreEndpointRecovers prevents a
// successful core request from hiding another endpoint's current failure.
func TestReactiveStoreKeepsWarningUntilEveryCoreEndpointRecovers(t *testing.T) {
	path, err := filepath.Abs("web/static/common.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs");
const vm = require("vm");
const deferred = new Map();
let streamResolve;
const encoder = new TextEncoder();
const reader = { read: () => new Promise((resolve) => { streamResolve = resolve; }) };
const toast = { textContent: "", replaceChildren(...nodes) { this.textContent = nodes.map((node) => node.textContent || node).join(""); } };
const status = { textContent: "", className: "" };
function deferredFetch(url) {
  if (url === "/ui/api/events") return Promise.resolve({ ok: true, status: 200, body: { getReader: () => reader } });
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  deferred.set(url, { promise, resolve });
  return promise;
}
const document = {
  title: "",
  querySelector(selector) {
    if (selector === 'meta[name="agentlink-token"]') return { content: "test" };
    if (selector === 'meta[name="agentlink-strings"]') return { content: "{}" };
    return null;
  },
  querySelectorAll() { return []; },
  getElementById(id) { return id === "toast-region" ? toast : (id === "status" ? status : null); },
  createElement() { return { textContent: "", addEventListener() {} }; },
  createTextNode(text) { return { textContent: text }; },
};
const source = fs.readFileSync(process.argv[1], "utf8");
vm.runInNewContext(source, { document, fetch: deferredFetch, setTimeout() {}, TextDecoder, TextEncoder, location: { reload() {} }, Map, Set, Object, Promise, Error, JSON }, { filename: process.argv[1] });
const response = (ok, status, body) => ({ ok, status, text: async () => body });
const flush = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };
(async () => {
  await flush();
  streamResolve({ value: encoder.encode('event: change\ndata: {"revision":0,"topics":["all"]}\n\n'), done: false });
  await flush();
  deferred.get("/ui/api/dashboard").resolve(response(false, 500, '{"error":"dashboard down"}'));
  await flush();
  if (toast.textContent !== "dashboard down") throw new Error("failure did not show in the shared banner: " + toast.textContent);
  deferred.get("/ui/api/status").resolve(response(true, 200, '{"configured":false}'));
  await flush();
  if (toast.textContent !== "dashboard down") throw new Error("successful status request cleared dashboard failure: " + toast.textContent);
})().catch((error) => { console.error(error.stack); process.exitCode = 1; });
`
	//nolint:gosec // G204: the fixed Node executable runs this test's embedded harness against the repository script.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("reactive store regression: %v\n%s", err, output)
	}
}

// TestReactivePushRefreshesOnlyEventTopics executes the browser state layer.
// It catches timer-driven API reads and broad refreshes after a narrow event.
func TestReactivePushRefreshesOnlyEventTopics(t *testing.T) {
	path, err := filepath.Abs("web/static/common.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs");
const vm = require("vm");
const calls = [];
let streamResolve;
const encoder = new TextEncoder();
const reader = { read: () => new Promise((resolve) => { streamResolve = resolve; }) };
const document = {
  title: "",
  querySelector(selector) {
    if (selector === 'meta[name="agentlink-token"]') return { content: "test-token" };
    if (selector === 'meta[name="agentlink-strings"]') return { content: "{}" };
    return null;
  },
  querySelectorAll() { return []; },
  getElementById() { return { textContent: "", className: "", replaceChildren() {} }; },
  createElement() { return { textContent: "", addEventListener() {} }; },
  createTextNode(text) { return { textContent: text }; },
};
async function fetch(url, options) {
  calls.push({ url, options });
  if (url === "/ui/api/events") return { ok: true, status: 200, body: { getReader: () => reader } };
  return { ok: true, status: 200, text: async () => "{}" };
}

const source = fs.readFileSync(process.argv[1], "utf8");
vm.runInNewContext(source, {
  document, fetch, TextDecoder, TextEncoder, AbortController, Map, Set, Object, Promise, Error, JSON,
  location: { reload() {} }, setTimeout() { throw new Error("scheduled data read"); }, clearTimeout() {},
}, { filename: process.argv[1] });
const flush = async () => { for (let i = 0; i < 50; i++) await Promise.resolve(); };
(async () => {
  await flush();
  if (calls[0].url !== "/ui/api/events") throw new Error("event stream was not opened first");
  if (calls[0].options.headers["X-Agentlink-Token"] !== "test-token") throw new Error("event stream token missing from header");
  streamResolve({ value: encoder.encode('event: change\ndata: {"revision":0,"topics":["all"]}\n\n'), done: false });
  await flush();
  const initial = calls.slice(1).map((call) => call.url).sort();
  const wanted = ["dashboard", "participants", "settings", "status", "threads", "update"].map((name) => "/ui/api/" + name).sort();
  if (JSON.stringify(initial) !== JSON.stringify(wanted)) throw new Error("initial slices: " + JSON.stringify(initial));
  calls.length = 0;
  streamResolve({ value: encoder.encode('event: change\ndata: {"revision":1,"topics":["threads"]}\n\n'), done: false });
  await flush();
  if (calls.length !== 1 || calls[0].url !== "/ui/api/threads") throw new Error("narrow event refreshed " + JSON.stringify(calls));
})().catch((error) => { console.error(error.stack); process.exitCode = 1; });
`
	//nolint:gosec // G204: fixed Node executable runs a repository script in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("push reactivity regression: %v\n%s", err, output)
	}
}

func TestReactivePushReconnectsWithBoundedBackoff(t *testing.T) {
	path, err := filepath.Abs("web/static/common.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs");
const vm = require("vm");
const calls = [];
const timers = [];
const document = {
  title: "",
  querySelector(selector) {
    if (selector === 'meta[name="agentlink-token"]') return { content: "header-secret" };
    if (selector === 'meta[name="agentlink-strings"]') return { content: "{}" };
    return null;
  },
  querySelectorAll() { return []; },
  getElementById() { return { textContent: "", className: "", replaceChildren() {} }; },
  createElement() { return { textContent: "", addEventListener() {} }; },
  createTextNode(text) { return { textContent: text }; },
};
async function fetch(url, options) { calls.push({ url, options }); throw new Error("offline"); }
function setTimeout(fn, delay) { timers.push({ fn, delay }); return timers.length; }
const source = fs.readFileSync(process.argv[1], "utf8");
vm.runInNewContext(source, { document, fetch, setTimeout, TextDecoder, Map, Set, Object, Promise, Error, JSON, location: { reload() {} } }, { filename: process.argv[1] });
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
(async () => {
  await flush();
  if (timers.length !== 1 || timers[0].delay !== 500) throw new Error("first reconnect: " + JSON.stringify(timers));
  timers.shift().fn();
  await flush();
  if (timers.length !== 1 || timers[0].delay !== 1000) throw new Error("second reconnect: " + JSON.stringify(timers));
  if (calls.length !== 2 || calls.some((call) => call.url !== "/ui/api/events" || call.options.headers["X-Agentlink-Token"] !== "header-secret")) {
    throw new Error("reconnect requests: " + JSON.stringify(calls));
  }
})().catch((error) => { console.error(error.stack); process.exitCode = 1; });
`
	//nolint:gosec // G204: fixed Node executable runs a repository script in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("push reconnect regression: %v\n%s", err, output)
	}
}

// TestPagesServeRussianText renders the application shell and its scripts and
// checks that they carry the dictionary and no English user-visible text.
func TestPagesServeRussianText(t *testing.T) {
	h := newHarness(t)
	client := *h.srv.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	})
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/dashboard" {
		t.Fatalf("root redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, path := range []string{"/ui/dashboard", "/ui/inbox", "/ui/participants", "/ui/settings"} {
		code, body := h.do(t, http.MethodGet, path, "", nil)
		if code != http.StatusOK || !strings.Contains(body, `id="app-shell"`) || !strings.Contains(body, h.app.token) {
			t.Fatalf("%s: %d", path, code)
		}
	}
	paths := []string{"/ui/static/common.js", "/ui/static/app.js", "/ui/static/overview.js", "/ui/static/inbox.js", "/ui/static/participants.js", "/ui/static/settings.js"}
	for _, p := range paths {
		code, body := h.do(t, http.MethodGet, p, "", nil)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", p, code)
		}
		for _, word := range englishOut {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(body) {
				t.Errorf("%s still shows English text %q", p, word)
			}
		}
	}
	for _, page := range []string{"/ui/dashboard", "/ui/inbox", "/ui/participants", "/ui/settings"} {
		_, body := h.do(t, http.MethodGet, page, "", nil)
		if strings.Contains(body, "{{STRINGS}}") {
			t.Fatalf("%s: dictionary not substituted", page)
		}
		if !strings.Contains(body, "&#34;nav.settings&#34;:&#34;Настройки&#34;") {
			t.Fatalf("%s: dictionary missing from the page", page)
		}
	}
}

// --- request/reply pairing ---

func entry(dir, id, from, to, body string, opts ...func(*node.Entry)) node.Entry {
	e := node.Entry{Direction: dir, Peer: to}
	if dir == "in" {
		e.Peer = from
	}
	e.Message = node.Message{ID: id, From: from, To: to, Body: body, CreatedAt: time.Unix(0, 0).UTC()}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func at(sec int64) func(*node.Entry) {
	return func(e *node.Entry) { e.CreatedAt = time.Unix(sec, 0).UTC() }
}
func replyTo(id string) func(*node.Entry) { return func(e *node.Entry) { e.ReplyTo = id } }
func job(s string) func(*node.Entry)      { return func(e *node.Entry) { e.JobStatus = s } }
func state(s string) func(*node.Entry)    { return func(e *node.Entry) { e.Status = s } }

func TestThreadsPairQuestionAndAnswer(t *testing.T) {
	req := strings.Repeat("a", 32)
	other := strings.Repeat("b", 32)
	cases := []struct {
		name    string
		entries []node.Entry
		want    []Thread
	}{{
		name: "outbound question with the peer's answer",
		entries: []node.Entry{
			entry("in", other, "nikita", "morgott", "готово", at(20), replyTo(req), job(node.JobCompleted)),
			entry("out", req, "morgott", "nikita", "что в файле?", at(10), state("sent"), job(node.JobCompleted)),
		},
		want: []Thread{{
			ID: req, Direction: "out", From: "morgott", To: "nikita", Body: "что в файле?",
			Status: node.JobCompleted, Answer: "готово", Answered: true,
		}},
	}, {
		name: "inbound question answered from this machine",
		entries: []node.Entry{
			entry("out", other, "morgott", "nikita", "мой ответ", at(30), replyTo(req), state("queued")),
			entry("in", req, "nikita", "morgott", "вопрос", at(10), state("delivered")),
		},
		want: []Thread{{
			ID: req, Direction: "in", From: "nikita", To: "morgott", Body: "вопрос",
			Status: statusAnswered, Answer: "мой ответ", Answered: true,
		}},
	}, {
		name: "unanswered inbound question can be replied to",
		entries: []node.Entry{
			entry("in", req, "nikita", "morgott", "вопрос", at(10), state("pending")),
		},
		want: []Thread{{
			ID: req, Direction: "in", From: "nikita", To: "morgott", Body: "вопрос",
			Status: "pending", Replyable: true,
		}},
	}, {
		name: "status updates never make a row",
		entries: []node.Entry{
			func() node.Entry {
				e := entry("in", other, "nikita", "morgott", "", at(15), replyTo(req), job(node.JobRunning))
				e.Kind = node.KindStatus
				return e
			}(),
			entry("out", req, "morgott", "nikita", "вопрос", at(10), state("sent"), job(node.JobRunning)),
		},
		want: []Thread{{
			ID: req, Direction: "out", From: "morgott", To: "nikita", Body: "вопрос",
			Status: node.JobRunning,
		}},
	}, {
		name: "a reply whose question is not listed keeps its own row",
		entries: []node.Entry{
			entry("in", other, "nikita", "morgott", "ответ", at(20), replyTo(req), job(node.JobFailed)),
		},
		want: []Thread{{
			ID: other, Direction: "in", From: "nikita", To: "morgott", Body: "ответ",
			Status: node.JobFailed,
		}},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := threads(c.entries)
			if len(got) != len(c.want) {
				t.Fatalf("got %d threads, want %d: %+v", len(got), len(c.want), got)
			}
			for i, w := range c.want {
				g := got[i]
				g.CreatedAt, g.AnswerAt = time.Time{}, nil
				if g != w {
					t.Errorf("thread %d:\n got %+v\nwant %+v", i, g, w)
				}
			}
		})
	}
}

func TestThreadsKeepTheNewestAnswerAndOrder(t *testing.T) {
	first, second, older := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	got := threads([]node.Entry{
		entry("out", first, "morgott", "nikita", "новый вопрос", at(100), state("queued")),
		entry("out", older, "morgott", "nikita", "старый вопрос", at(10), state("sent")),
		entry("in", second, "nikita", "morgott", "первый ответ", at(20), replyTo(older), job(node.JobRunning)),
		entry("in", strings.Repeat("d", 32), "nikita", "morgott", "второй ответ", at(30), replyTo(older), job(node.JobCompleted)),
	})
	if len(got) != 2 || got[0].ID != first || got[1].ID != older {
		t.Fatalf("order: %+v", got)
	}
	if got[1].Answer != "второй ответ" || got[1].Status != node.JobCompleted {
		t.Fatalf("newest answer not kept: %+v", got[1])
	}
	if got[1].AnswerAt == nil || !got[1].AnswerAt.Equal(time.Unix(30, 0).UTC()) {
		t.Fatalf("answer_at: %+v", got[1].AnswerAt)
	}
}

// A running question shows the peer agent's activity and the silence mark;
// an answered one shows neither.
func TestThreadsCarryActivityAndSilence(t *testing.T) {
	running, answered := strings.Repeat("a", 32), strings.Repeat("b", 32)
	live := func(e *node.Entry) { e.Activity, e.NoNewsMin = "Read docs/index.md", 7 }
	got := threads([]node.Entry{
		entry("out", running, "morgott", "nikita", "вопрос", at(100), job(node.JobRunning), live),
		entry("out", answered, "morgott", "nikita", "другой", at(50), job(node.JobCompleted), live, func(e *node.Entry) { e.Answer = "ok" }),
	})
	if got[0].Activity != "Read docs/index.md" || got[0].NoNewsMin != 7 {
		t.Fatalf("running thread: %+v", got[0])
	}
	if got[1].Activity != "" || got[1].NoNewsMin != 0 {
		t.Fatalf("answered thread: %+v", got[1])
	}
}

func TestThreadsEndpointServesPairs(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(t, http.MethodGet, "/ui/api/threads", "", h.tokenHdr()); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("threads before setup: %d %s", code, body)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/send", `{"to":"bob","body":"вопрос"}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	code, body := h.do(t, http.MethodGet, "/ui/api/threads", "", h.tokenHdr())
	var got []Thread
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != http.StatusOK {
		t.Fatalf("threads: %d %s err=%v", code, body, err)
	}
	if len(got) != 1 || got[0].Direction != "out" || got[0].From != "alice" || got[0].To != "bob" ||
		got[0].Body != "вопрос" || got[0].Answered {
		t.Fatalf("thread %+v", got)
	}
}

func TestThreadsEndpointFiltersDecodedPeerAndUsesCompleteHistory(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	for i := range 205 {
		body := fmt.Sprintf(`{"to":"bob","body":"вопрос %d"}`, i)
		if code, got := h.do(t, http.MethodPost, "/ui/api/send", body, h.tokenHdr()); code != http.StatusOK {
			t.Fatalf("send %d: %d %s", i, code, got)
		}
	}
	// Network peer names intentionally cannot contain spaces or '&'. Move the
	// real queued messages only inside this temporary store to prove URL-decoded
	// filtering also handles a history value containing both.
	peer := "карл & sons"
	dataDir := filepath.Join(filepath.Dir(h.path), "data", "outbox")
	if err := os.Rename(filepath.Join(dataDir, "bob"), filepath.Join(dataDir, peer)); err != nil {
		t.Fatal(err)
	}
	code, body := h.do(t, http.MethodGet, "/ui/api/threads?peer="+url.QueryEscape(peer), "", h.tokenHdr())
	var got []Thread
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != http.StatusOK || len(got) != 205 {
		t.Fatalf("encoded peer: %d threads, status %d, err=%v, body=%s", len(got), code, err, body)
	}
	for _, thread := range got {
		if thread.To != peer {
			t.Fatalf("thread peer %q, want %q", thread.To, peer)
		}
	}
	code, body = h.do(t, http.MethodGet, "/ui/api/threads?peer=", "", h.tokenHdr())
	var all []Thread
	if err := json.Unmarshal([]byte(body), &all); err != nil || code != http.StatusOK || len(all) != 200 {
		t.Fatalf("empty peer: %d %s err=%v", code, body, err)
	}
	code, body = h.do(t, http.MethodGet, "/ui/api/threads?peer=%26%3F%23", "", h.tokenHdr())
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("reserved peer: %d %s, want empty filtered result", code, body)
	}
}

func TestDashboardAPIsServeStoppedNodeDefaults(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(t, http.MethodGet, "/ui/api/dashboard", "", h.tokenHdr())
	var dashboard DashboardSummary
	if err := json.Unmarshal([]byte(body), &dashboard); err != nil || code != http.StatusOK ||
		dashboard.SentMessages != 0 || dashboard.ReceivedMessages != 0 || dashboard.TotalMessages != 0 ||
		dashboard.ActiveRequests != 0 || len(dashboard.Recent) != 0 || dashboard.Status.Running {
		t.Fatalf("dashboard: %d %s err=%v", code, body, err)
	}
	if !strings.Contains(body, `"recent":[]`) {
		t.Fatalf("dashboard recent is not an array: %s", body)
	}
	code, body = h.do(t, http.MethodGet, "/ui/api/participants", "", h.tokenHdr())
	var participants []ParticipantView
	if err := json.Unmarshal([]byte(body), &participants); err != nil || code != http.StatusOK || len(participants) != 0 {
		t.Fatalf("participants: %d %s err=%v", code, body, err)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("participants are not an array: %s", body)
	}
}
