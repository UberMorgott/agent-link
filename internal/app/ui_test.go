package app

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// The web UI's behavior (chat, settings, participants, the event stream) is
// covered by its own tests: internal/app/web, npm test. These tests cover what
// the Go side owns: the dictionary, the served pages and the API.

// --- the page dictionary ---

var (
	// A dictionary key in the UI sources: t("inbox.send"), or t('inbox.send')
	// inside a template attribute. A double-quoted attribute value itself
	// (v-model="form.code") is an expression, not a key.
	sourceKey = regexp.MustCompile(`(?:^|[^=])"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"|'([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)'`)
	// A key built at run time: "settings.handler." + name.
	sourcePrefix = regexp.MustCompile(`["']([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)*\.)["']\s*\+`)
	scriptKey    = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)
	dynPrefix    = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)*\.)"\s*\+`)
	cyrillic     = regexp.MustCompile(`\p{Cyrillic}`)
	englishOut   = []string{
		"Settings", "Inbox", "Save", "Send", "Reply", "Replying", "Generate", "Copy", "Show",
		"This computer", "The other person", "Answering requests", "Shared secret", "Handler agent",
		"Working folder", "My name", "Their name", "My address", "Their address", "Message",
		"not configured", "offline", "is not running", "Queued", "Not sent", "Not saved", "Saved",
		"Could not load", "None", "Cancel",
	}
)

// uiSources returns the web UI's own source files (web/src, tests aside) by path.
func uiSources(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	root, err := os.OpenRoot(filepath.Join("web", "src"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	src := root.FS()
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := path.Ext(p)
		if (ext != ".ts" && ext != ".vue") || strings.HasSuffix(p, ".test.ts") {
			return nil
		}
		data, err := fs.ReadFile(src, p)
		files["web/src/"+p] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no web UI sources under web/src")
	}
	return files
}

// TestUIStringsCoverPages checks that the UI and the dictionary agree: no key
// used by the UI is missing, and no dictionary entry is dead.
func TestUIStringsCoverPages(t *testing.T) {
	used := map[string]bool{}
	prefixes := map[string]bool{}
	for name, body := range uiSources(t) {
		for _, m := range sourceKey.FindAllStringSubmatch(body, -1) {
			key := m[1] + m[2]
			if _, ok := uiStrings[key]; !ok {
				t.Errorf("%s uses key %q, missing from uiStrings", name, key)
			}
			used[key] = true
		}
		for _, m := range sourcePrefix.FindAllStringSubmatch(body, -1) {
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

// TestUISourcesCarryNoEnglishText: every visible word comes from the
// dictionary, so the UI sources name no English label.
func TestUISourcesCarryNoEnglishText(t *testing.T) {
	for name, body := range uiSources(t) {
		for _, word := range englishOut {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(body) {
				t.Errorf("%s still shows English text %q", name, word)
			}
		}
	}
}

// --- the served pages ---

var assetRef = regexp.MustCompile(`(?:src|href)="(/ui/assets/[^"]+)"`)

// TestPagesServeShell: the root and the removed overview page redirect to the
// inbox, every route serves the one application shell with the token, the
// dictionary, the version and a CSP nonce filled in, and the shell's scripts
// and styles are served.
func TestPagesServeShell(t *testing.T) {
	h := newHarness(t, func(a *App) { a.Version = "0.7.1" })
	client := *h.srv.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	for _, path := range []string{"/", "/ui/dashboard"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/inbox" {
			t.Fatalf("%s redirect: %d %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	nonceMeta := regexp.MustCompile(`<meta name="agentlink-nonce" content="([A-Za-z0-9+/=]{16,})">`)
	nonces := map[string]bool{}
	for _, path := range []string{"/ui/inbox", "/ui/agents", "/ui/participants", "/ui/settings"} {
		code, header, body := h.get(t, path)
		if code != http.StatusOK || !strings.Contains(body, `<div id="app">`) {
			t.Fatalf("%s: %d", path, code)
		}
		if strings.Count(body, h.app.token) != 1 || !strings.Contains(body, `<meta name="agentlink-token" content="`+h.app.token+`">`) {
			t.Fatalf("%s: token must be in its meta tag exactly once", path)
		}
		if strings.Contains(body, "{{") {
			t.Fatalf("%s: a placeholder is left unfilled", path)
		}
		if !strings.Contains(body, "&#34;nav.settings&#34;:&#34;Настройки&#34;") {
			t.Fatalf("%s: dictionary missing from the page", path)
		}
		if !strings.Contains(body, `<meta name="agentlink-version" content="0.7.1">`) {
			t.Fatalf("%s: shell does not carry the build version", path)
		}
		m := nonceMeta.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no CSP nonce in the page", path)
		}
		csp := header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "style-src 'self' 'nonce-"+m[1]+"'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: CSP %q does not admit the page's nonce", path, csp)
		}
		if header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: shell must not be cached: %q", path, header.Get("Cache-Control"))
		}
		nonces[m[1]] = true
	}
	if len(nonces) != 4 {
		t.Fatalf("every page load needs its own nonce: %d distinct of 4", len(nonces))
	}
	_, _, body := h.get(t, "/ui/inbox")
	assets := assetRef.FindAllStringSubmatch(body, -1)
	if len(assets) < 2 {
		t.Fatalf("shell references %d assets, want its script and stylesheet", len(assets))
	}
	for _, m := range append(assets, []string{"", "/ui/icon.svg"}) {
		code, header, _ := h.get(t, m[1])
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", m[1], code)
		}
		if strings.HasPrefix(m[1], "/ui/assets/") && !strings.Contains(header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: hashed asset is not cached as immutable: %q", m[1], header.Get("Cache-Control"))
		}
	}
	for _, path := range []string{"/ui/nope", "/ui/assets/missing.js", "/ui/static/app.js", "/ui/index.html"} {
		if code, _, _ := h.get(t, path); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
}

// get fetches a path without the token and returns its status, headers and body.
func (h *harness) get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(data)
}

// TestDistIsTheOnlyEmbeddedTree: the binary carries the built UI and nothing of
// its sources or dependencies.
func TestDistIsTheOnlyEmbeddedTree(t *testing.T) {
	err := fs.WalkDir(webFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." && p != "web" && !strings.HasPrefix(p, "web/dist") {
			t.Errorf("embedded %s outside web/dist", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web/dist/index.html", "web/dist/open.html", "web/dist/icon.svg"} {
		if _, err := webFS.ReadFile(name); err != nil {
			t.Errorf("%s missing from the embedded UI: %v", name, err)
		}
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

// TestSessionsEndpoint: the chat's waiting line reads this computer's live
// sessions; a stopped node has none, as an empty list.
func TestSessionsEndpoint(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(t, http.MethodGet, "/ui/api/sessions", "", nil); code != http.StatusForbidden {
		t.Fatalf("sessions without the token: %d", code)
	}
	code, body := h.do(t, http.MethodGet, "/ui/api/sessions", "", h.tokenHdr())
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("sessions of a stopped node: %d %s", code, body)
	}
}

func TestParticipantsAPIServesStoppedNodeDefaults(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(t, http.MethodGet, "/ui/api/dashboard", "", h.tokenHdr()); code != http.StatusNotFound {
		t.Fatalf("removed dashboard API: %d, want 404", code)
	}
	code, body := h.do(t, http.MethodGet, "/ui/api/participants", "", h.tokenHdr())
	var participants []ParticipantView
	if err := json.Unmarshal([]byte(body), &participants); err != nil || code != http.StatusOK || len(participants) != 0 {
		t.Fatalf("participants: %d %s err=%v", code, body, err)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("participants are not an array: %s", body)
	}
}
