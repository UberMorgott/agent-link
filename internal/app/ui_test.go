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
	for _, card := range []string{"identity", "handler", "application", "projects", "updates", "advanced"} {
		if !settingsCards[card] {
			t.Errorf("settings card %q is missing", card)
		}
	}
}

// settingsHarnessJS is the fake DOM settings.js runs against in Node.
const settingsHarnessJS = `
class Element { constructor(id=""){this.id=id;this.value="";this.checked=false;this.hidden=false;this.disabled=false;this.textContent="";this.className="";this.listeners={};this.open=false;this.children=[]} addEventListener(n,f){this.listeners[n]=f} setAttribute(n,v){this[n]=v} removeAttribute(n){delete this[n]} select(){} focus(){} append(...c){this.children.push(...c)} replaceChildren(...c){this.children=[...c]} }
const names=["form","settings_result","code","work_dir","pick","agent_path","pick_agent","find_agent","agent_row","agent_shown","work_dir_shown","work_dir_hooks","hooks_codex","advanced","my_addr","generate","copy","projects","projects_empty","add_project","updates","update_text","update_check","update_apply","update_auto","update_version"];
const elements=Object.fromEntries(names.map((id)=>[id,new Element(id)]));
const controls=Object.fromEntries(["node","code","handler","agent_path","work_dir","listen","api","areas","discovery","max_jobs","autostart"].map((name)=>[name,new Element(name)]));
controls.handler.value="none"; controls.discovery.checked=true; elements.form.elements=controls;`

// TestSettingsProjectRowsRenderAndSave runs settings.js: saved projects become
// cards, «Добавить проект» adds one, a picked folder and «Удалить» save at once
// as "projects", a half-typed row is never sent, and a repeated area is not
// sent but named in a sentence.
func TestSettingsProjectRowsRenderAndSave(t *testing.T) {
	path, err := filepath.Abs("web/static/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs"), vm = require("vm");
` + settingsHarnessJS + `
const document={getElementById:(id)=>elements[id],createElement:()=>new Element()};
const state={settings:{node:"n",areas:["site"],projects:{site:{dir:"E:\\site"}}},status:null,dashboard:null,update:null};
const store={get:()=>state,subscribe(){},patch(name,value){state[name]=value}};
const sent=[]; let picked="E:\\docs";
async function api(method,path,body){if(path==="settings"){sent.push(body);return {saved:true,settings:body}}if(path==="pick-folder")return {path:picked};if(path==="agent")return {text:""};return {current:"dev",enabled:false}}
const t=(key)=>key,fmt=(key)=>key;
vm.runInNewContext(fs.readFileSync(process.argv[1],"utf8"),{document,store,api,t,fmt,crypto:{},navigator:{},location:{reload(){}},Array,Number,Object,Promise,RegExp,String,Uint8Array,console});
const list=elements.projects;
const parts=(li)=>({area:li.children[0].children[1],dir:li.children[1].children[1].children[0],pick:li.children[1].children[1].children[1],hooks:li.children[2],remove:li.children[3],count:li.children.length});
const flush=async()=>{for(let i=0;i<8;i++)await Promise.resolve()};
const edit=async(input,value)=>{input.value=value;elements.form.listeners.input({target:input});elements.form.listeners.change({target:input});await flush()};
(async()=>{
if(list.children.length!==1||!elements.projects_empty.hidden)throw new Error("saved project not rendered");
let first=parts(list.children[0]);
if(first.area.value!=="site"||first.dir.value!=="E:\\site")throw new Error("row values: "+JSON.stringify([first.area.value,first.dir.value]));
if(first.count!==4)throw new Error("a project row must hold only area, folder, hook status and remove: "+first.count);
await edit(first.dir,"");
if(sent.length!==0)throw new Error("a row without a folder was sent: "+JSON.stringify(sent));
await edit(first.dir,"E:\\site");
if(sent.length!==0)throw new Error("an unchanged form was sent again");
elements.add_project.listeners.click();
if(list.children.length!==2||sent.length!==0)throw new Error("add did not add a row, or saved an empty one");
const second=parts(list.children[1]);
await edit(second.area," docs ");
if(sent.length!==0)throw new Error("a row without a folder was sent");
await second.pick.listeners.click(); await flush();
if(second.dir.value!=="E:\\docs")throw new Error("folder picker did not fill the row");
const want={site:{dir:"E:\\site"},docs:{dir:"E:\\docs"}};
if(sent.length!==1||JSON.stringify(sent[0].projects)!==JSON.stringify(want))throw new Error("picked folder did not save: "+JSON.stringify(sent));
await first.remove.listeners.click(); await flush();
if(list.children.length!==1)throw new Error("remove did not drop the row");
if(sent.length!==2||JSON.stringify(sent[1].projects)!==JSON.stringify({docs:{dir:"E:\\docs"}}))throw new Error("remove did not save: "+JSON.stringify(sent[1]));
elements.add_project.listeners.click();
const third=parts(list.children[1]);
third.dir.value="E:\\other";
await edit(third.area,"docs");
if(sent.length!==2||elements.settings_result.textContent!=="error.projects_twice")throw new Error("duplicate area was sent: "+JSON.stringify(sent));
})().catch((error)=>{console.error(error.stack);process.exitCode=1});`
	//nolint:gosec // G204: fixed Node executable runs a checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("settings projects: %v\n%s", err, output)
	}
}

// TestSettingsSavePatchesReactiveSlices executes the real browser module: a
// changed field saves by itself, with no save button. It catches
// timer-style/broad rereads after a mutation, lost response slices,
// and reloads that are not caused by an API-address change.
func TestSettingsSavePatchesReactiveSlices(t *testing.T) {
	path, err := filepath.Abs("web/static/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs"), vm = require("vm");
` + settingsHarnessJS + `
const document={getElementById:(id)=>elements[id],createElement:()=>new Element()};
const state={settings:{node:"old",api:"127.0.0.1:7520",areas:[]},status:null,dashboard:null,update:null};
const patches=[]; const store={get:()=>state,subscribe(){},patch(name,value){state[name]=value;patches.push(name)}};
let reloads=0, refreshes=0;
let saveResponse={saved:true,settings:{node:"saved",api:"127.0.0.1:7520",areas:["dev"]},status:{configured:true,node:"saved"},dashboard:{total_messages:7}};
async function api(method,path,body){if(path==="settings")return saveResponse;if(path==="agent")return {text:""};return {current:"dev",enabled:false}}
function refreshSlice(){refreshes++;throw new Error("save performed a broad refresh")}
const t=(key)=>key,fmt=(key)=>key; const crypto={getRandomValues:(x)=>x}; const navigator={clipboard:{writeText:async()=>{}}};
const location={reload(){reloads++}};
vm.runInNewContext(fs.readFileSync(process.argv[1],"utf8"),{document,store,api,refreshSlice,t,fmt,crypto,navigator,location,Array,Number,Object,Promise,RegExp,String,Uint8Array,console});
const change=async(input)=>{elements.form.listeners.input({target:input});elements.form.listeners.change({target:input});for(let i=0;i<8;i++)await Promise.resolve()};
(async()=>{if(elements.settings_save)throw new Error("settings still have a save button");controls.node.value="saved";controls.api.value="127.0.0.1:7520";controls.areas.value="dev";await change(controls.areas);
if(refreshes!==0)throw new Error("refreshSlice called "+refreshes+" times");
if(JSON.stringify(patches)!==JSON.stringify(["settings","status","dashboard"]))throw new Error("patches: "+JSON.stringify(patches));
if(state.settings.node!=="saved"||state.status.node!=="saved"||state.dashboard.total_messages!==7)throw new Error("response slices were not applied");
if(reloads!==0)throw new Error("same API address reloaded the page");
patches.length=0; controls.node.value="request-only"; saveResponse={saved:true}; await change(controls.node);
if(patches.length||state.settings.node!=="saved")throw new Error("request body was patched without response fields");
controls.api.value=""; saveResponse={saved:true,settings:{node:"saved",api:"127.0.0.1:7520",areas:[]}}; await change(controls.api);
if(reloads!==0)throw new Error("clearing API reloaded despite normalized server value");
controls.api.value="127.0.0.1:7599"; saveResponse={saved:true,settings:{node:"saved",api:"127.0.0.1:7599",areas:[]}}; await change(controls.api);
if(reloads!==1)throw new Error("changed effective API did not reload exactly once: "+reloads);
})().catch((error)=>{console.error(error.stack);process.exitCode=1});`
	//nolint:gosec // G204: fixed Node executable runs a checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("settings reactive mutation regression: %v\n%s", err, output)
	}
}

// TestInboxChatControlsAreLabelled keeps the chat choices usable: the project
// area of a new chat and the "who must answer" chooser carry visible labels.
func TestInboxChatControlsAreLabelled(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(webFiles(t)["web/app.html"]))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*html.Node{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, attr := range n.Attr {
				if attr.Key == "id" {
					byID[attr.Val] = n
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	area := byID["new_chat_area"]
	if area == nil || area.Parent == nil || area.Parent.Data != "label" || strings.Contains(attr(area.Parent, "class"), "sr-only") {
		t.Fatal("#new_chat_area must have a visible label")
	}
	for _, group := range []string{"ask_row", "new_chat_members"} {
		n := byID[group]
		for n != nil && attr(n, "role") != "group" {
			n = n.Parent
		}
		if n == nil || byID[attr(n, "aria-labelledby")] == nil {
			t.Errorf("#%s is not inside a labelled group", group)
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

func TestParticipantRowsAreKeyboardFocusableChatControls(t *testing.T) {
	path, err := filepath.Abs("web/static/participants.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs=require("fs"),vm=require("vm");
class Element { constructor(id="",tag="DIV"){this.id=id;this.tagName=tag.toUpperCase();this.tabIndex=0;this.value="";this.hidden=false;this.disabled=false;this.textContent="";this.className="";this.children=[];this.listeners={}} append(...nodes){this.children.push(...nodes)} replaceChildren(...nodes){this.children=[...nodes]} addEventListener(name,fn){this.listeners[name]=fn} setAttribute(name,value){this[name]=value} removeAttribute(name){delete this[name]} focus(){this.focused=true} }
const ids=["participants","participants_result","participant_addr","participant_add","add_participant","participants_empty"];
const elements=Object.fromEntries(ids.map((id)=>[id,new Element(id)]));
const document={getElementById:(id)=>elements[id],createElement:(tag)=>new Element("",tag)};
const state={participants:null}; const store={get:()=>state,subscribe(){},patch(name,value){state[name]=value}};
const t=(key)=>key,fmt=(key,vars)=>key+JSON.stringify(vars); let navigation=null;
function navigate(route,query){navigation={route,query}} async function api(){return {}} function confirm(){return true}
const context=vm.createContext({document,store,t,fmt,navigate,api,confirm,Date,Array,Object,Promise,String,console});
vm.runInContext(fs.readFileSync(process.argv[1],"utf8"),context);
vm.runInContext('renderParticipants([{name:"bob",online:true,total:2,sent:1,received:1}])',context);
const row=elements.participants.children[0],open=row.children[0];
if(open.tagName!=="BUTTON"||open.type!=="button"||open.tabIndex<0)throw new Error("participant row is not a keyboard-focusable button");
if(!String(open["aria-label"]||"").includes("bob"))throw new Error("participant row has no accessible name");
open.listeners.click();
if(!navigation||navigation.route!=="inbox"||navigation.query.peer!=="bob")throw new Error("participant keyboard control does not open chat");`
	//nolint:gosec // G204: fixed Node executable runs the checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("participant keyboard regression: %v\n%s", err, output)
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
  const wanted = ["chats", "dashboard", "participants", "settings", "status", "update"].map((name) => "/ui/api/" + name).sort();
  if (JSON.stringify(initial) !== JSON.stringify(wanted)) throw new Error("initial slices: " + JSON.stringify(initial));
  calls.length = 0;
  streamResolve({ value: encoder.encode('event: change\ndata: {"revision":1,"topics":["chats"]}\n\n'), done: false });
  await flush();
  if (calls.length !== 1 || calls[0].url !== "/ui/api/chats") throw new Error("narrow event refreshed " + JSON.stringify(calls));
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

// TestVersionReachesPageAndEveryAPIResponse: the shell names the build it was
// served by, and API responses name the running build even when they refuse an
// old tab's token, so a tab open across a self-update can tell.
func TestVersionReachesPageAndEveryAPIResponse(t *testing.T) {
	h := newHarness(t, func(a *App) { a.Version = "0.7.1" })
	if _, body := h.do(t, http.MethodGet, "/ui/settings", "", nil); !strings.Contains(body, `<meta name="agentlink-version" content="0.7.1">`) {
		t.Fatal("shell does not carry the build version")
	}
	for name, hdr := range map[string]map[string]string{"token": h.tokenHdr(), "old token": {TokenHeader: "old"}} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/ui/api/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get(VersionHeader); got != "0.7.1" {
			t.Errorf("%s: %s = %q, want 0.7.1", name, VersionHeader, got)
		}
	}
}

// TestPageReloadsOnlyForAnotherBuild runs common.js: an event stream answered
// by another build reloads the page, a reconnect to the same build does not.
func TestPageReloadsOnlyForAnotherBuild(t *testing.T) {
	path, err := filepath.Abs("web/static/common.js")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
const fs = require("fs");
const vm = require("vm");
const source = fs.readFileSync(process.argv[1], "utf8");
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
function run(serverVersion, status) {
  const state = { reloads: 0, timers: [] };
  const document = {
    title: "",
    querySelector(selector) {
      if (selector === 'meta[name="agentlink-token"]') return { content: "t" };
      if (selector === 'meta[name="agentlink-strings"]') return { content: "{}" };
      if (selector === 'meta[name="agentlink-version"]') return { content: "0.7.0" };
      return null;
    },
    querySelectorAll() { return []; },
    getElementById() { return { textContent: "", className: "", replaceChildren() {} }; },
    createElement() { return { textContent: "", addEventListener() {} }; },
    createTextNode(text) { return { textContent: text }; },
  };
  const headers = { get: (name) => (name === "X-Agentlink-Version" ? serverVersion : null) };
  const reader = { read: async () => ({ done: true }) };
  async function fetch() { return { ok: status === 200, status, headers, body: { getReader: () => reader } }; }
  vm.runInNewContext(source, {
    document, fetch, TextDecoder, Map, Set, Object, Promise, Error, JSON,
    setTimeout(fn, delay) { state.timers.push(delay); return state.timers.length; },
    location: { reload() { state.reloads++; } },
  }, { filename: process.argv[1] });
  return state;
}
(async () => {
  const restarted = run("0.7.1", 403);
  const updated = run("0.7.1", 200);
  const same = run("0.7.0", 200);
  const sameRestarted = run("0.7.0", 403);
  await flush();
  if (restarted.reloads !== 1 || updated.reloads !== 1) throw new Error("new build did not reload: " + restarted.reloads + " " + updated.reloads);
  if (same.reloads !== 0 || sameRestarted.reloads !== 0) throw new Error("same build reloaded the page");
  if (same.timers.length !== 1) throw new Error("same build did not reconnect: " + JSON.stringify(same.timers));
})().catch((error) => { console.error(error.stack); process.exitCode = 1; });
`
	//nolint:gosec // G204: fixed Node executable runs a repository script in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path).CombinedOutput(); err != nil {
		t.Fatalf("version reload regression: %v\n%s", err, output)
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

// inboxHarnessJS is the fake DOM and app globals inbox.js runs against in
// Node. Elements are created on first lookup by id.
const inboxHarnessJS = `
const fs = require("fs");
class ClassList {
  constructor(el) { this.el = el; }
  toggle(name, on) { const set = new Set(this.el.className.split(/\s+/).filter(Boolean)); if (on === undefined ? !set.has(name) : on) set.add(name); else set.delete(name); this.el.className = [...set].join(" "); return set.has(name); }
  contains(name) { return this.el.className.split(/\s+/).includes(name); }
}
class Element {
  constructor(id = "", tagName = "DIV") { this.id = id; this.tagName = tagName.toUpperCase(); this.tabIndex = 0; this.value = ""; this.hidden = false; this.disabled = false; this.checked = false; this.textContent = ""; this.className = ""; this.dataset = {}; this.style = { props: {}, setProperty(k, v) { this.props[k] = v; } }; this.children = []; this.listeners = {}; this.scrollTop = 0; this.scrollHeight = 0; this.clientHeight = 100; this.selectionStart = 0; this.selectionEnd = 0; this.replacements = 0; this.parentElement = null; this.classList = new ClassList(this); }
  detach(node) { if (node.parentElement) node.parentElement.children = node.parentElement.children.filter((item) => item !== node); node.parentElement = this; }
  append(...nodes) { for (const node of nodes) { this.detach(node); this.children.push(node); } this.scrollHeight = this.children.length * 100; }
  insertBefore(node, before) { this.detach(node); const index = before ? this.children.indexOf(before) : -1; if (index < 0) this.children.push(node); else this.children.splice(index, 0, node); this.scrollHeight = this.children.length * 100; }
  replaceChildren(...nodes) { this.replacements++; for (const child of this.children) child.parentElement = null; this.children = []; this.append(...nodes); }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  setAttribute(name, value) { this[name] = value; }
  removeAttribute(name) { delete this[name]; }
  focus() { document.activeElement = this; }
  scrollIntoView() { this.scrolled = true; }
  get childElementCount() { return this.children.length; }
  get firstElementChild() { return this.children[0] || null; }
  get lastElementChild() { return this.children[this.children.length - 1] || null; }
  remove() { this.removed = true; if (this.parentElement) { this.parentElement.children = this.parentElement.children.filter((node) => node !== this); this.parentElement = null; } }
}
const elements = {};
const document = {
  activeElement: null,
  getElementById: (id) => elements[id] || (elements[id] = new Element(id)),
  createElement: (tagName) => new Element("", tagName),
};
const text = (node) => [node.textContent || "", ...(node.children || []).map(text)].join(" ");
const state = { status: { node: "local", members: [{ name: "local", self: true, online: true }, { name: "bob", online: true }, { name: "карл & sons", online: true }] }, settings: { areas: ["dev"] }, chats: null, chatArchive: null, showArchive: false, chat: null, selectedChat: "", selectedMessage: "", drafts: {} };
const listeners = new Map();
const store = {
  get: () => state,
  patch(name, value) { state[name] = value; for (const fn of listeners.get(name) || []) fn(value, state); },
  subscribe(name, fn) { if (!listeners.has(name)) listeners.set(name, new Set()); listeners.get(name).add(fn); },
};
const dict = { "inbox.activity.type.edit": "правит" };
const t = (key) => dict[key] || key;
const fmt = (key, vars) => key + JSON.stringify(vars);
let navigation = null;
function navigate(route, query) { navigation = { route, query }; }
const refreshed = [];
function refreshSlice(name) { refreshed.push(name); }
function confirm() { return true; }
const localStorage = { values: new Map(), getItem(k) { return this.values.has(k) ? this.values.get(k) : null; }, setItem(k, v) { this.values.set(k, v); } };
const timers = new Map(); let timerSeq = 0;
function setTimeout(fn, ms) { timerSeq++; timers.set(timerSeq, { fn, ms }); return timerSeq; }
function clearTimeout(id) { timers.delete(id); }
function runTimers() { for (const [id, timer] of [...timers]) { timers.delete(id); timer.fn(); } }
const intervals = [];
function setInterval(fn, ms) { intervals.push({ fn, ms }); return intervals.length; }
const flush = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
`

// inboxRun executes inbox.js followed by testSource in one context.
func inboxRun(t *testing.T, name, fixtures, testSource string) {
	t.Helper()
	path, err := filepath.Abs("web/static/inbox.js")
	if err != nil {
		t.Fatal(err)
	}
	// The fixtures run in the page context: api() and the data it serves are
	// globals there, like the real one from common.js.
	program := inboxHarnessJS + `
const source = fs.readFileSync(process.argv[1], "utf8");
const test = fs.readFileSync(process.argv[2], "utf8");
require("vm").runInNewContext(test.split("\n//TEST\n")[0] + "\n" + source + "\n;(async () => {\n" + test.split("\n//TEST\n")[1] + "\n})().catch((error) => { console.error(error.stack); process.exitCode = 1; });", { document, store, t, fmt, navigate, refreshSlice, confirm, localStorage, setTimeout, clearTimeout, setInterval, runTimers, timers, intervals, elements, text, navigation: () => navigation, refreshed, flush, console, process, URLSearchParams, Date, Map, Set, Object, Array, Promise, JSON, String, Number, Math, Error, decodeURIComponent }, { filename: process.argv[1] });
`
	testPath := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(testPath, []byte(fixtures+"\n//TEST\n"+testSource), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G204: fixed Node executable runs the checked-in browser module in a deterministic harness.
	if output, err := exec.CommandContext(t.Context(), "node", "-e", program, path, testPath).CombinedOutput(); err != nil {
		t.Fatalf("%s regression: %v\n%s", name, err, output)
	}
}

// TestInboxChatState runs the inbox against fake chat endpoints. It catches
// rebuilding unchanged bubbles, losing a draft/caret or the scroll position on
// a refresh, an unescaped chat id, sending twice, timers that ask the app for
// data, and a chat list without its open/closed, unread and activity marks.
func TestInboxChatState(t *testing.T) {
	const fixtures = `
const calls = [];
const base = 1700000000000;
const iso = (ms) => new Date(ms).toISOString();
const group = "c1 &";
const history = [{ id: "open", seq: 1, kind: "chat_open", from: "local", body: "", created_at: iso(base), direction: "out" }]
  .concat(Array.from({ length: 205 }, (_, i) => ({ id: "m" + i, seq: i + 2, from: i % 2 ? "bob" : "local", direction: i % 2 ? "in" : "out", body: "line " + i, created_at: iso(base + i * 1000) })));
const chats = {
  [group]: {
    info: { id: group, participants: ["bob", "local", "карл & sons"], title: "line 0", closed: false, archived: false, last_seq: 206, last_at: iso(base + 204000),
      members: [
        { name: "bob", connected: true, compatible: true, queued: 0, jobs: [{ reply_to: "m204", job_status: "running", activity_info: { type: "edit", text: "app.go", phase: "running", started_at: iso(base + 241000) }, updated_at: iso(base + 241000) }], held: [{ reply_to: "m202", job_status: "held", hold_reason: "no_handler", activity: "никто не отвечает — ждёт человека" }] },
        { name: "local", self: true, connected: true, compatible: true, queued: 0 },
        { name: "карл & sons", connected: false, compatible: true, queued: 2, jobs: [{ reply_to: "m204", job_status: "queued", updated_at: iso(base + 204000), stale: true }] },
      ] },
    items: history,
  },
  c2: { info: { id: "c2", participants: ["alice", "local"], closed: true, closed_by: "alice", closed_at: iso(base), archived: true, last_seq: 3, members: [] }, items: [] },
};
const control = { releaseSend: null, sent: [] };
async function api(method, path, body) {
  calls.push(method + " " + path);
  if (method === "POST" && path === "send") { control.sent.push(body); return new Promise((resolve) => { control.releaseSend = () => resolve({ id: "s1" }); }); }
  const m = path.match(/^chats\/([^/?]+)(\/messages)?(?:\?(.*))?$/);
  const chat = m && chats[decodeURIComponent(m[1])];
  if (!chat) throw new Error("unexpected call " + method + " " + path);
  if (!m[2]) return chat.info;
  const q = new URLSearchParams(m[3] || "");
  const limit = Number(q.get("limit"));
  if (q.get("after")) return chat.items.filter((x) => x.seq > Number(q.get("after"))).slice(0, limit);
  const before = Number(q.get("before") || 0);
  const older = chat.items.filter((x) => !before || x.seq < before);
  return older.slice(Math.max(0, older.length - limit));
}
`
	const testSource = `
const list = elements.messages;
await selectChat(group, "m204");
if (!calls.includes("GET chats/c1%20%26") || !calls.includes("GET chats/c1%20%26/messages?limit=200")) throw new Error("chat id query: " + JSON.stringify(calls));
if (list.children.length !== 201 || list.children[0].className !== "msg-older") throw new Error("timeline: " + list.children.length + " " + list.children[0].className);
const anchor = list.children.find((node) => node.dataset.messageId === "m204");
if (!anchor || !anchor.scrolled) throw new Error("message anchor was not revealed");
const heldBubble = list.children.find((node) => node.dataset.messageId === "m202");
const holdNote = heldBubble.children.find((c) => c.className === "msg-meta msg-hold");
if (holdNote.hidden || text(holdNote) !== "bob: никто не отвечает — ждёт человека") throw new Error("held note: " + text(holdNote));
if (!anchor.children.find((c) => c.className === "msg-meta msg-hold").hidden) throw new Error("hold note on a message nobody held");
if (!heldBubble.children.find((c) => c.className === "msg-reply").hidden) throw new Error("own message offers a reply");
if (!text(elements.conversation_title).includes("bob, карл & sons")) throw new Error("title: " + elements.conversation_title.textContent);
if (elements.chat_members.children.length !== 3 || elements.chat_close.hidden || elements.send.hidden) throw new Error("header or composer of an open chat");
if (!elements.chat_members.children[2].className.includes("away") || !text(elements.chat_members.children[2]).includes("inbox.member.queued")) throw new Error("away member chip: " + text(elements.chat_members.children[2]));
if (elements.ask_choices.children.some((label) => label.children[0].checked) || elements.ask_hint.hidden) throw new Error("a group chat must not ask anyone by default");

// Live activity: one row per job, local timers, no app calls.
const dock = elements.chat_activity;
if (dock.hidden || dock.children.length !== 2) throw new Error("activity rows: " + dock.children.length);
const bobRow = dock.children[0], karlRow = dock.children[1];
if (!text(bobRow).includes("правит app.go") || !karlRow.className.includes("stale") || !text(karlRow).includes("inbox.activity.stale")) throw new Error("activity text: " + text(bobRow) + " | " + text(karlRow));
const realNow = Date.now;
Date.now = () => base + 246000;
const before = calls.length;
if (intervals.length !== 1 || intervals[0].ms !== 1000) throw new Error("one 1s timer expected: " + JSON.stringify(intervals));
intervals[0].fn();
const total = bobRow.children.find((c) => c.className === "act-time");
if (total.textContent !== "· 0:42" || bobRow.children.some((c) => c.className === "act-step")) throw new Error("one timer expected: " + text(bobRow));
Date.now = () => base + 306000;
intervals[0].fn();
if (total.textContent !== "· 1:42" || calls.length !== before) throw new Error("timer tick: " + total.textContent + " calls " + (calls.length - before));
Date.now = realNow;

// A refresh keeps nodes, focus, draft, caret and an upward scroll.
const kept = list.children[11];
const replyControl = kept.children.find((c) => c.className === "msg-reply");
replyControl.focus();
await loadChat(group, false);
if (list.children[11] !== kept || kept.children.find((c) => c.className === "msg-reply") !== replyControl || document.activeElement !== replyControl) throw new Error("unchanged bubbles were rebuilt");
elements.body.value = "first\nsecond"; elements.body.selectionStart = 3; elements.body.selectionEnd = 3; elements.body.focus();
list.scrollTop = 45; list.clientHeight = 100; list.scrollHeight = 20500;
chats[group].items[20] = { ...chats[group].items[20], body: "edited" };
await loadChat(group, false);
if (list.children[11] !== kept) throw new Error("bubble replaced on refresh");
if (elements.body.value !== "first\nsecond" || document.activeElement !== elements.body || elements.body.selectionStart !== 3) throw new Error("draft focus/caret changed");
if (list.scrollTop !== 45) throw new Error("upward scroll jumped: " + list.scrollTop);
list.scrollTop = 20400; list.scrollHeight = 20500;
chats[group].items.push({ id: "m205", seq: 207, from: "bob", direction: "in", body: "new", created_at: iso(base + 300000) });
await loadChat(group, false);
if (list.scrollTop !== list.scrollHeight) throw new Error("a view at the bottom did not follow a new message");

// Older page, then reply + send with the chosen responder, guarded against a double submit.
await list.children[0].children[0].listeners.click();
if (list.children.length !== 207 || list.children[0].className === "msg-older") throw new Error("older page: " + list.children.length);
const bobBubble = list.children.find((node) => node.dataset.messageId === "m203");
bobBubble.children.find((c) => c.className === "msg-reply").listeners.click();
if (elements.replying.hidden || elements.reply_to.value !== "m203") throw new Error("reply target");
const asked = elements.ask_choices.children.filter((label) => label.children[0].checked).map((label) => label.children[0].value);
if (JSON.stringify(asked) !== JSON.stringify(["bob"])) throw new Error("reply must ask its author: " + JSON.stringify(asked));
elements.body.value = "first\nsecond";
elements.send.listeners.submit({ preventDefault() {} }); elements.send.listeners.submit({ preventDefault() {} });
await flush();
if (calls.filter((c) => c === "POST send").length !== 1 || !elements.send_button.disabled) throw new Error("duplicate send guard");
if (JSON.stringify(control.sent[0]) !== JSON.stringify({ chat_id: group, body: "first\nsecond", ask: ["bob"], reply_to: "m203" })) throw new Error("send body: " + JSON.stringify(control.sent[0]));
control.releaseSend(); await flush();
if (elements.send_button.disabled || elements.body.value !== "" || !elements.replying.hidden) throw new Error("composer after send");

// The list: open/closed badges, unread, live activity; rows are buttons.
const second = { ...chats.c2.info, last_seq: 3, last_message: { id: "a1", from: "alice", direction: "in", body: "old from alice" } };
store.patch("chats", [chats[group].info, second]);
store.patch("chats", [chats[group].info, { ...second, last_seq: 4, last_message: { id: "a2", from: "alice", direction: "in", body: "latest from alice" } }]);
const rows = elements.conversation_list.children;
const groupText = text(rows[0]), aliceText = text(rows[1]);
if (!groupText.includes("inbox.badge.open") || !groupText.includes("inbox.working") || groupText.includes("inbox.unread")) throw new Error("group row: " + groupText);
if (!aliceText.includes("latest from alice") || !aliceText.includes("inbox.unread") || !aliceText.includes("inbox.badge.closed")) throw new Error("alice row: " + aliceText);
const aliceButton = rows[1].children[0];
if (aliceButton.tagName !== "BUTTON" || aliceButton.tabIndex < 0) throw new Error("chat row is not keyboard focusable");
aliceButton.listeners.click();
if (navigation().route !== "inbox" || navigation().query.chat !== "c2") throw new Error("row navigation: " + JSON.stringify(navigation()));

// A closed chat is readable but has no composer; a peer without an open chat gets the new-chat form.
await selectChat("c2", "");
if (!elements.send.hidden || elements.chat_note.hidden || !elements.chat_close.hidden) throw new Error("closed chat controls");
openInbox({ peer: "alice" });
if (elements.new_chat_form.hidden || !elements.new_chat_members.children.some((label) => label.children[0].value === "alice" && label.children[0].checked)) throw new Error("peer link did not preselect a new chat");
store.patch("chats", [{ id: "c3", participants: ["bob", "local"], closed: false, last_seq: 0, members: [] }, ...store.get().chats]);
openInbox({ peer: "bob" });
if (navigation().query.chat !== "c3") throw new Error("peer link did not open the existing chat: " + JSON.stringify(navigation()));
`
	inboxRun(t, "chat-state", fixtures, testSource)
}

// TestInboxNotificationWatermark proves that existing messages are seeded
// silently and only a later incoming message creates one safe, bounded toast.
func TestInboxNotificationWatermark(t *testing.T) {
	const fixtures = `
const calls = [];
async function api() { return {}; }
const chat = (id, msg) => ({ id, participants: ["local", msg.from], last_seq: 1, members: [], last_message: msg });
const initial = [chat("c-old", { id: "old", from: "bob", direction: "in", body: "old" })];
const long = "😀".repeat(121);
const next = [chat("c-new", { id: "new", from: "карл & sons", direction: "in", body: long }), ...initial, chat("c-out", { id: "out", from: "local", direction: "out", body: "ignore" })];
`
	const testSource = `
const region = document.getElementById("message-toast-region");
processIncomingChats(initial);
if (region.children.length) throw new Error("initial history produced a toast");
processIncomingChats(next);
if (region.children.length !== 1) throw new Error("new inbound toast count: " + region.children.length);
const toast = region.children[0];
if (!text(toast).includes("карл & sons")) throw new Error("sender missing: " + text(toast));
const main = toast.children.find((child) => child.className === "message-toast-main");
const preview = main.children.find((child) => child.className === "message-toast-preview").textContent;
if (Array.from(preview.replace(/…$/, "")).length !== 120 || !preview.endsWith("…")) throw new Error("preview not code-point bounded: " + Array.from(preview).length);
const close = toast.children.find((child) => child.className === "message-toast-close");
if (!close || close.tagName !== "BUTTON" || close["aria-label"] !== "inbox.toast.close") throw new Error("toast has no labelled close button");
if (timers.size !== 1) throw new Error("toast did not arm an auto-dismiss timer: " + timers.size);
main.listeners.click();
if (navigation().route !== "inbox" || navigation().query.chat !== "c-new" || navigation().query.message !== "new") throw new Error("toast navigation: " + JSON.stringify(navigation()));
if (region.children.length !== 0 || timers.size !== 0) throw new Error("opened toast stayed on screen");
const saved = JSON.parse(localStorage.values.get("agentlink.notifications.v1:local"));
if (!saved.includes("old") || !saved.includes("new") || saved.includes("out")) throw new Error("watermark: " + JSON.stringify(saved));
store.get().selectedChat = "c-old";
processIncomingChats([chat("c-old", { id: "old2", from: "bob", direction: "in", body: "seen here" }), ...next]);
if (region.children.length !== 0) throw new Error("the open chat produced a toast");
const many = [...next, ...Array.from({ length: 4 }, (_, i) => chat("c" + i, { id: "bulk" + i, from: "peer" + i, direction: "in", body: "bulk " + i }))];
processIncomingChats(many);
if (region.childElementCount !== 3) throw new Error("visible toast limit: " + region.childElementCount);
if ([...timers.values()].some((timer) => timer.ms !== 6000)) throw new Error("auto-dismiss timeout: " + JSON.stringify([...timers.values()].map((timer) => timer.ms)));
region.children[0].children.find((child) => child.className === "message-toast-close").listeners.click();
if (region.childElementCount !== 2 || timers.size !== 2) throw new Error("close button did not dismiss the toast");
runTimers();
if (region.childElementCount !== 0) throw new Error("toasts did not auto-dismiss: " + region.childElementCount);
`
	inboxRun(t, "notification-watermark", fixtures, testSource)
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
