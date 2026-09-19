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

func TestSettingsHasNoParticipantsAndParticipantsOwnControls(t *testing.T) {
	files := webFiles(t)
	body := files["web/app.html"]
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	views := map[string]*html.Node{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, attr := range n.Attr {
				if attr.Key == "data-view" {
					views[attr.Val] = n
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	has := func(root *html.Node, attr, value string) bool {
		found := false
		var inspect func(*html.Node)
		inspect = func(n *html.Node) {
			if n.Type == html.ElementNode {
				for _, a := range n.Attr {
					if a.Key == attr && a.Val == value {
						found = true
					}
				}
			}
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				inspect(child)
			}
		}
		inspect(root)
		return found
	}
	participants, settings := views["participants"], views["settings"]
	for _, control := range []struct{ attr, value string }{
		{"id", "participants"}, {"id", "participant_addr"}, {"id", "add_participant"}, {"id", "participants_result"},
	} {
		if participants == nil || !has(participants, control.attr, control.value) {
			t.Errorf("participants view is missing %s=%q", control.attr, control.value)
		}
	}
	for _, legacy := range []struct{ attr, value string }{
		{"id", "members"}, {"id", "add_peer"}, {"name", "peer_addr"},
	} {
		if settings != nil && has(settings, legacy.attr, legacy.value) {
			t.Errorf("settings still contains participant control %s=%q", legacy.attr, legacy.value)
		}
	}
}

// TestUISemanticContracts protects the persistent application shell from
// turning cards into fake form groups or losing keyboard/screen-reader affordances.
func TestUISemanticContracts(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(webFiles(t)["web/app.html"]))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	hasLabel := func(n *html.Node) bool {
		if attr(n, "aria-label") != "" || attr(n, "aria-labelledby") != "" {
			return true
		}
		for parent := n.Parent; parent != nil; parent = parent.Parent {
			if parent.Type == html.ElementNode && parent.Data == "label" {
				return true
			}
		}
		return false
	}
	views := map[string]int{}
	viewH1 := map[string]int{}
	settingsCards := map[string]bool{}
	labelledNav, liveRegion, fieldsets := false, false, 0
	var walk func(*html.Node, string)
	walk = func(n *html.Node, view string) {
		if n.Type == html.ElementNode {
			if current := attr(n, "data-view"); current != "" {
				view = current
				views[view]++
			}
			if n.Data == "h1" && view != "" {
				viewH1[view]++
			}
			if n.Data == "nav" && (attr(n, "aria-label") != "" || attr(n, "aria-labelledby") != "") {
				labelledNav = true
			}
			if attr(n, "aria-live") != "" {
				liveRegion = true
			}
			if n.Data == "fieldset" {
				fieldsets++
			}
			if card := attr(n, "data-settings-card"); card != "" {
				settingsCards[card] = true
			}
			if (n.Data == "input" || n.Data == "select" || n.Data == "textarea") && attr(n, "type") != "hidden" && !hasLabel(n) {
				t.Errorf("unlabelled %s#%s[name=%s]", n.Data, attr(n, "id"), attr(n, "name"))
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child, view)
		}
	}
	walk(doc, "")
	for _, view := range []string{"dashboard", "inbox", "participants", "settings"} {
		if views[view] != 1 || viewH1[view] != 1 {
			t.Errorf("view %q: sections=%d h1=%d, want one each", view, views[view], viewH1[view])
		}
	}
	if !labelledNav || !liveRegion {
		t.Errorf("labelled navigation=%v live region=%v, want both", labelledNav, liveRegion)
	}
	if fieldsets != 0 {
		t.Errorf("found %d visual fieldsets; cards must use sections", fieldsets)
	}
	for _, card := range []string{"identity", "handler", "application", "updates", "advanced"} {
		if !settingsCards[card] {
			t.Errorf("settings card %q is missing", card)
		}
	}
}

// TestSettingsSavePatchesReactiveSlices executes the real browser module and
// catches timer-style/broad rereads after a mutation, lost response slices,
// and reloads that are not caused by an API-address change.
func TestSettingsSavePatchesReactiveSlices(t *testing.T) {
	path, err := filepath.Abs("web/static/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs"), vm = require("vm");
class Element { constructor(id=""){this.id=id;this.value="";this.checked=false;this.hidden=false;this.disabled=false;this.textContent="";this.className="";this.listeners={};this.open=false} addEventListener(n,f){this.listeners[n]=f} setAttribute(n,v){this[n]=v} removeAttribute(n){delete this[n]} select(){} }
const names=["form","settings_result","settings_save","code","work_dir","pick","agent_path","pick_agent","find_agent","agent_row","agent_shown","work_dir_shown","advanced","my_addr","generate","copy","updates","update_text","update_check","update_apply","update_auto","update_version"];
const elements=Object.fromEntries(names.map((id)=>[id,new Element(id)]));
const controls=Object.fromEntries(["node","code","handler","agent_path","work_dir","listen","api","areas","discovery","max_jobs","autostart"].map((name)=>[name,new Element(name)]));
controls.handler.value="none"; controls.discovery.checked=true; elements.form.elements=controls;
const document={getElementById:(id)=>elements[id]};
const state={settings:{node:"old",api:"127.0.0.1:7520",areas:[]},status:null,dashboard:null,update:null};
const patches=[]; const store={get:()=>state,subscribe(){},patch(name,value){state[name]=value;patches.push(name)}};
let reloads=0, refreshes=0;
async function api(method,path,body){if(path==="settings")return {saved:true,settings:{...body,node:"saved"},status:{configured:true,node:"saved"},dashboard:{total_messages:7}};if(path==="agent")return {text:""};return {current:"dev",enabled:false}}
function refreshSlice(){refreshes++;throw new Error("save performed a broad refresh")}
const t=(key)=>key,fmt=(key)=>key; const crypto={getRandomValues:(x)=>x}; const navigator={clipboard:{writeText:async()=>{}}};
const location={reload(){reloads++}};
vm.runInNewContext(fs.readFileSync(process.argv[1],"utf8"),{document,store,api,refreshSlice,t,fmt,crypto,navigator,location,Array,Number,Object,Promise,RegExp,String,Uint8Array,console});
(async()=>{controls.node.value="saved";controls.api.value="127.0.0.1:7520";controls.areas.value="dev";await elements.form.listeners.submit({preventDefault(){}});for(let i=0;i<8;i++)await Promise.resolve();
if(refreshes!==0)throw new Error("refreshSlice called "+refreshes+" times");
if(JSON.stringify(patches)!==JSON.stringify(["settings","status","dashboard"]))throw new Error("patches: "+JSON.stringify(patches));
if(state.settings.node!=="saved"||state.status.node!=="saved"||state.dashboard.total_messages!==7)throw new Error("response slices were not applied");
if(reloads!==0)throw new Error("same API address reloaded the page");
})().catch((error)=>{console.error(error.stack);process.exitCode=1});`
	//nolint:gosec // G204: fixed Node executable runs a checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("settings reactive mutation regression: %v\n%s", err, output)
	}
}

func TestInboxKeepsAccessibleAreaRecipientChooser(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(webFiles(t)["web/app.html"]))
	if err != nil {
		t.Fatal(err)
	}
	var recipient *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" {
			for _, attr := range n.Attr {
				if attr.Key == "id" && attr.Val == "to" {
					recipient = n
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if recipient == nil || recipient.Parent == nil || recipient.Parent.Data != "label" {
		t.Fatal("#to must have a visible label")
	}
	for _, attr := range recipient.Parent.Attr {
		if attr.Key == "class" && strings.Contains(attr.Val, "sr-only") {
			t.Fatal("#to is hidden; area fan-out is not usable")
		}
	}
}

func TestParticipantControlsAreLocalizedAndGuardMutations(t *testing.T) {
	files := webFiles(t)
	htmlBody := files["web/app.html"]
	script := files["web/static/participants.js"]
	if strings.Contains(htmlBody, `placeholder="10.147.20.9"`) {
		t.Error("participant address placeholder is hard-coded in the template")
	}
	for _, contract := range []string{
		`participantAddr.placeholder = t("participants.add.placeholder")`,
		`fmt("participants.remove_named", { name: person.name })`,
		`participantAddButton.disabled = true`,
		`participantAddButton.disabled = false`,
		`remove.disabled = true`,
		`remove.disabled = false`,
		`setAttribute("aria-busy", "true")`,
		`removeAttribute("aria-busy")`,
	} {
		if !strings.Contains(script, contract) {
			t.Errorf("participants script is missing mutation/accessibility contract %q", contract)
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

// TestInboxConversationState executes the inbox browser module against a tiny
// DOM. It catches replacing message nodes, losing an in-progress draft/caret,
// using an unescaped peer query, and sending the same form twice.
func TestInboxConversationState(t *testing.T) {
	path, err := filepath.Abs("web/static/inbox.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs");
class Element {
  constructor(id = "") { this.id = id; this.value = ""; this.hidden = false; this.disabled = false; this.textContent = ""; this.className = ""; this.dataset = {}; this.children = []; this.listeners = {}; this.scrollTop = 0; this.scrollHeight = 0; this.clientHeight = 100; this.selectionStart = 0; this.selectionEnd = 0; this.replacements = 0; }
  append(...nodes) { for (const node of nodes) { if (node.parentElement) node.parentElement.children = node.parentElement.children.filter((item) => item !== node); node.parentElement = this; this.children.push(node); } this.scrollHeight = this.children.length * 100; }
  insertBefore(node, before) { if (node.parentElement) node.parentElement.children = node.parentElement.children.filter((item) => item !== node); node.parentElement = this; const index = before ? this.children.indexOf(before) : -1; if (index < 0) this.children.push(node); else this.children.splice(index, 0, node); this.scrollHeight = this.children.length * 100; }
  replaceChildren(...nodes) { this.replacements++; this.children = []; this.append(...nodes); }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  setAttribute(name, value) { this[name] = value; }
  removeAttribute(name) { delete this[name]; }
  focus() { document.activeElement = this; }
  scrollIntoView() { this.scrolled = true; }
  setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; }
  get childElementCount() { return this.children.length; }
  get firstElementChild() { return this.children[0] || null; }
  get lastElementChild() { return this.children[this.children.length - 1] || null; }
  remove() { this.removed = true; if (this.parentElement) this.parentElement.children = this.parentElement.children.filter((node) => node !== this); }
}
const ids = ["messages", "conversation_list", "send", "inbox_result", "reply_to", "replying", "replying_text", "to", "body", "cancel_reply", "send_button", "message-toast-region"];
const elements = Object.fromEntries(ids.map((id) => [id, new Element(id)]));
const document = {
  activeElement: null,
  getElementById: (id) => elements[id],
  createElement: () => new Element(),
  createTextNode: (text) => ({ textContent: text }),
};
const state = { selectedPeer: "", selectedMessage: "", drafts: {}, conversationReads: {}, threads: null, threadFeed: null, status: { node: "local" }, participants: [] };
const listeners = new Map();
const store = {
  get: () => state,
  patch(name, value) { state[name] = value; for (const fn of listeners.get(name) || []) fn(value, state); },
  subscribe(name, fn) { if (!listeners.has(name)) listeners.set(name, new Set()); listeners.get(name).add(fn); },
};
const calls = [];
const sentBodies = [];
const control = { releaseSend: null };
const full = Array.from({ length: 205 }, (_, i) => ({ id: "m" + i, direction: i % 2 ? "in" : "out", from: i % 2 ? "карл & sons" : "local", to: i % 2 ? "local" : "карл & sons", body: "line " + i, created_at: new Date(1700000000000 + i * 1000).toISOString(), status: "sent", replyable: i % 2 === 1 }));
async function api(method, path, body) {
  calls.push(method + " " + path);
  if (method === "GET") return full;
  sentBodies.push(body);
  return new Promise((resolve) => { control.releaseSend = () => resolve(full[204]); });
}
const t = (key) => key;
const fmt = (key, vars) => key + JSON.stringify(vars);
let navigation = null;
function navigate(route, query) { navigation = { route, query }; }
const localStorage = { values: new Map(), getItem(k) { return this.values.get(k) || null; }, setItem(k, v) { this.values.set(k, v); } };
const source = fs.readFileSync(process.argv[1], "utf8");
const test = fs.readFileSync(process.argv[2], "utf8");
require("vm").runInNewContext(source + test, { document, store, api, t, fmt, navigate, localStorage, calls, sentBodies, control, full, elements, console, process, URLSearchParams, Date, Map, Set, Object, Array, Promise, JSON, String }, { filename: process.argv[1] });
`
	const testSource = `
(async () => {
  await selectConversation("карл & sons", "m204");
  if (calls[0] !== "GET threads?peer=%D0%BA%D0%B0%D1%80%D0%BB%20%26%20sons") throw new Error("peer query: " + calls[0]);
  if (store.get().selectedPeer !== "карл & sons" || elements.messages.children.length !== 205) throw new Error("full conversation was not selected");
  if (!elements.messages.children[204].scrolled) throw new Error("message anchor was not revealed");
  const kept = elements.messages.children[11];
  const replyControl = kept.children[kept.children.length - 1];
  replyControl.focus();
  const replacements = kept.replacements;
  const unchanged = full.map((item) => ({ ...item }));
  renderTimeline(unchanged);
  if (kept.replacements !== replacements || kept.children[kept.children.length - 1] !== replyControl || document.activeElement !== replyControl) throw new Error("unchanged message controls were rebuilt");
  elements.body.value = "first\nsecond"; elements.body.selectionStart = 3; elements.body.selectionEnd = 3; elements.body.focus();
  elements.messages.scrollTop = 45; elements.messages.clientHeight = 100; elements.messages.scrollHeight = 20500;
  const changed = full.map((item) => ({ ...item })); changed[11].activity = "reading";
  renderTimeline(changed);
  if (elements.messages.children[11] !== kept || kept.children[kept.children.length - 1] !== replyControl) throw new Error("message controls were replaced");
  if (elements.body.value !== "first\nsecond" || document.activeElement !== elements.body || elements.body.selectionStart !== 3) throw new Error("draft focus/caret changed");
  if (elements.messages.scrollTop !== 45) throw new Error("upward scroll jumped: " + elements.messages.scrollTop);
  elements.to.value = "area:dev";
  elements.send.listeners.submit({ preventDefault() {} }); elements.send.listeners.submit({ preventDefault() {} });
  await Promise.resolve();
  if (calls.filter((call) => call === "POST send").length !== 1 || !elements.send_button.disabled || sentBodies[0].to !== "area:dev") throw new Error("area send or duplicate guard failed");
  control.releaseSend(); await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  if (elements.send_button.disabled) throw new Error("send button stayed disabled");
  store.patch("selectedPeer", "");
  store.patch("status", { node: "local", peer: "bob" });
  await Promise.resolve();
  if (store.get().selectedPeer !== "bob") throw new Error("first connected peer was not selected");
  store.patch("participants", [{ name: "bob", online: true, total: 4, latest_preview: "hello", latest_at: "2026-01-01T00:00:00Z", latest_direction: "out" }, { name: "alice", online: false, total: 2, latest_preview: "latest from alice", latest_at: "2026-01-02T00:00:00Z", latest_direction: "in" }]);
  store.patch("dashboard", { recent: [] });
  const aliceRow = elements.conversation_list.children[1];
  const rowText = (node) => [node.textContent, ...(node.children || []).flatMap((child) => rowText(child))];
  const aliceText = rowText(aliceRow).join(" ");
  if (!aliceText.includes("latest from alice") || !aliceText.includes("inbox.unread") || !aliceText.includes("inbox.peer_offline")) throw new Error("conversation summary missing: " + aliceText);
})().catch((error) => { console.error(error.stack); process.exitCode = 1; });`
	testPath := filepath.Join(t.TempDir(), "conversation-test.js")
	if err := os.WriteFile(testPath, []byte(testSource), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G204: fixed Node executable runs the checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path, testPath).CombinedOutput(); err != nil {
		t.Fatalf("conversation state regression: %v\n%s", err, output)
	}
}

// TestInboxNotificationWatermark proves that existing messages are seeded
// silently and only a later inbound request creates one safe, bounded toast.
func TestInboxNotificationWatermark(t *testing.T) {
	path, err := filepath.Abs("web/static/inbox.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs");
class Element { constructor(id="") { this.id=id; this.value=""; this.hidden=false; this.disabled=false; this.textContent=""; this.children=[]; this.listeners={}; this.dataset={}; } append(...x){for(const node of x){node.parentElement=this;this.children.push(node)}this.textContent=this.children.map((n)=>n.textContent||"").join("")} replaceChildren(...x){this.children=[];this.append(...x)} addEventListener(n,f){this.listeners[n]=f} setAttribute(){} focus(){} get childElementCount(){return this.children.length} get firstElementChild(){return this.children[0]||null} remove(){this.removed=true;if(this.parentElement){this.parentElement.children=this.parentElement.children.filter((node)=>node!==this);this.parentElement.textContent=this.parentElement.children.map((n)=>n.textContent||"").join("")}} }
const ids=["messages","conversation_list","send","inbox_result","reply_to","replying","replying_text","to","body","cancel_reply","send_button","message-toast-region"];
const elements=Object.fromEntries(ids.map((id)=>[id,new Element(id)]));
const document={activeElement:null,getElementById:(id)=>elements[id],createElement:()=>new Element(),createTextNode:(text)=>({textContent:text})};
const state={selectedPeer:"",selectedMessage:"",drafts:{},threads:null,threadFeed:null,status:{node:"local"},participants:[]};
const subscriptions=new Map(); const store={get:()=>state,patch(n,v){state[n]=v;for(const f of subscriptions.get(n)||[])f(v,state)},subscribe(n,f){if(!subscriptions.has(n))subscriptions.set(n,new Set());subscriptions.get(n).add(f)}};
const t=(key)=>key, fmt=(key,vars)=>key+JSON.stringify(vars); async function api(){return []}
const navState={}; function navigate(route,query){navState.value={route,query}}
const localStorage={values:new Map(),getItem(k){return this.values.get(k)||null},setItem(k,v){this.values.set(k,v)}};
const initial=[{id:"old",direction:"in",from:"bob",to:"local",body:"old",created_at:"2026-01-01T00:00:00Z",status:"pending"}];
const long="😀".repeat(121);
const next=[...initial,{id:"new",direction:"in",from:"карл & sons",to:"local",body:long,created_at:"2026-01-02T00:00:00Z",status:"pending"},{id:"out",direction:"out",from:"local",to:"bob",body:"ignore",created_at:"2026-01-03T00:00:00Z",status:"sent"}];
const source=fs.readFileSync(process.argv[1],"utf8");
const test=fs.readFileSync(process.argv[2],"utf8");
require("vm").runInNewContext(source+test,{document,store,api,t,fmt,navigate,localStorage,initial,next,elements,navState,console,process,URLSearchParams,Date,Map,Set,Object,Array,Promise,JSON,String},{filename:process.argv[1]});
`
	const testSource = `
processIncomingThreads(initial);
if(elements["message-toast-region"].children.length) throw new Error("initial history produced a toast");
processIncomingThreads(next);
if(elements["message-toast-region"].children.length!==1) throw new Error("new inbound toast count");
const toast=elements["message-toast-region"].children[0];
if(!toast.textContent.includes("карл & sons")) throw new Error("sender missing: "+toast.textContent);
const preview=toast.children.find((child)=>child.className==="message-toast-preview").textContent;
if(Array.from(preview.replace(/…$/,"" )).length!==120 || !preview.endsWith("…")) throw new Error("preview not code-point bounded: "+Array.from(preview).length);
toast.listeners.click();
if(navState.value.route!=="inbox" || navState.value.query.peer!=="карл & sons" || navState.value.query.message!=="new") throw new Error("toast navigation: "+JSON.stringify(navState.value));
const saved=JSON.parse(localStorage.values.get("agentlink.notifications.v1:local"));
if(!saved.includes("old") || !saved.includes("new") || saved.includes("out")) throw new Error("watermark: "+JSON.stringify(saved));
const answered=next.map((item)=>item.id==="out"?{...item,answered:true,answer:"reply from bob",answer_at:"2026-01-04T00:00:00Z"}:item);
processIncomingThreads(answered);
if(elements["message-toast-region"].children.length!==2) throw new Error("new reply did not produce a toast");
const replyToast=elements["message-toast-region"].children[1];
if(!replyToast.textContent.includes("bob") || !replyToast.textContent.includes("reply from bob")) throw new Error("reply toast content: "+replyToast.textContent);
replyToast.listeners.click();
if(navState.value.query.peer!=="bob" || navState.value.query.message!=="out") throw new Error("reply toast target: "+JSON.stringify(navState.value));
const many=[...answered,...Array.from({length:4},(_,i)=>({id:"bulk"+i,direction:"in",from:"peer"+i,to:"local",body:"bulk "+i,created_at:new Date(1760000000000+i*1000).toISOString(),status:"pending"}))];
processIncomingThreads(many);
if(elements["message-toast-region"].childElementCount!==3) throw new Error("visible toast limit: "+elements["message-toast-region"].childElementCount);`
	testPath := filepath.Join(t.TempDir(), "notification-test.js")
	if err := os.WriteFile(testPath, []byte(testSource), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G204: fixed Node executable runs the checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path, testPath).CombinedOutput(); err != nil {
		t.Fatalf("notification watermark regression: %v\n%s", err, output)
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
