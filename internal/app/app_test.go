package app

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

type harness struct {
	app       *App
	srv       *httptest.Server
	path      string
	autostart []bool
}

// newHarness serves a fresh App; setup runs before the server starts.
func newHarness(t *testing.T, setup ...func(*App)) *harness {
	t.Helper()
	h := &harness{path: filepath.Join(t.TempDir(), "config.json")}
	a, err := New(h.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.SetAutostart = func(on bool) error { h.autostart = append(h.autostart, on); return nil }
	a.AutostartState = nil // the saved setting; never the real registry
	a.Discovery = false    // tests find each other by address only
	for _, f := range setup {
		f(a)
	}
	h.app = a
	h.srv = httptest.NewServer(a.Handler())
	t.Cleanup(func() { h.srv.Close(); a.Stop() })
	return h
}

func (h *harness) do(t *testing.T, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, strings.NewReader(body))
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
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (h *harness) tokenHdr() map[string]string { return map[string]string{TokenHeader: h.app.token} }

func validJSON(t *testing.T) string {
	s := settings.Settings{
		Node: "alice", Listen: "127.0.0.1:0", PeerName: "bob", PeerAddr: "127.0.0.1:1",
		Code: "abc123", Areas: []string{"dev"}, Handler: worker.HandlerNone, Autostart: true,
	}
	data, err := json.Marshal(s) //nolint:gosec // G117: test fixture, Secret is empty
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPageEmbedsToken(t *testing.T) {
	h := newHarness(t)
	for _, page := range []string{"/ui/settings", "/ui/inbox"} {
		code, body := h.do(t, http.MethodGet, page, "", nil)
		if code != http.StatusOK || !strings.Contains(body, `<meta name="agentlink-token" content="`+h.app.token+`">`) {
			t.Fatalf("%s: status %d, token embedded=%v", page, code, strings.Contains(body, h.app.token))
		}
	}
}

// TestOpenPageIsPublicLauncher: /ui/open carries no token or dictionary and
// loads only its own same-origin launcher module (web/src/open.ts).
func TestOpenPageIsPublicLauncher(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(t, http.MethodGet, "/ui/open", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /ui/open: status %d", code)
	}
	if strings.Contains(body, h.app.token) || strings.Contains(body, "agentlink-token") || strings.Contains(body, "agentlink-strings") {
		t.Fatal("launcher contains private application data")
	}
	script := regexp.MustCompile(`<script type="module" crossorigin src="(/ui/assets/open-[^"]+\.js)"></script>`).FindStringSubmatch(body)
	if strings.Count(body, "<script") != 1 || script == nil {
		t.Fatalf("launcher must load only its same-origin module: %s", body)
	}
	if code, _ := h.do(t, http.MethodGet, script[1], "", nil); code != http.StatusOK {
		t.Fatalf("%s: status %d", script[1], code)
	}
}

func TestAPITokenGuard(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no token", nil, http.StatusForbidden},
		{"wrong token", map[string]string{TokenHeader: strings.Repeat("0", 64)}, http.StatusForbidden},
		{"foreign origin", map[string]string{TokenHeader: h.app.token, "Origin": "http://evil.example"}, http.StatusForbidden},
		{"cross-site fetch", map[string]string{TokenHeader: h.app.token, "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same origin", map[string]string{TokenHeader: h.app.token, "Origin": h.srv.URL, "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, _ := h.do(t, http.MethodGet, "/ui/api/settings", "", c.hdr); code != c.want {
				t.Fatalf("GET settings: %d, want %d", code, c.want)
			}
			if code, _ := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), c.hdr); c.want == http.StatusForbidden && code != c.want {
				t.Fatalf("POST settings: %d, want %d", code, c.want)
			}
		})
	}
}

func TestDashboardAPIsRequireToken(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/ui/api/dashboard", "/ui/api/participants", "/ui/api/threads?peer=bob"} {
		if code, _ := h.do(t, http.MethodGet, path, "", nil); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", path, code)
		}
	}
}

func TestRebindingHostRejected(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/ui/settings", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

func TestSaveStartsNodeAndPersists(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(t, http.MethodGet, "/inbox", "", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("control API before setup: %d, want 503", code)
	}
	code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr())
	if code != http.StatusOK || !strings.Contains(body, `"saved":true`) || strings.Contains(body, "error") {
		t.Fatalf("save: %d %s", code, body)
	}
	if st := h.app.Status(); !st.Configured || !st.Running || st.Node != "alice" {
		t.Fatalf("status %+v", st)
	}
	if len(h.autostart) != 1 || !h.autostart[0] {
		t.Fatalf("autostart calls %v", h.autostart)
	}
	s, ok, err := settings.Load(h.path)
	if err != nil || !ok || s.Node != "alice" || s.Code != "ABC123" || s.Secret != "" {
		t.Fatalf("persisted %+v ok=%v err=%v", s, ok, err)
	}
	// The CLI control API is served once the node runs, and still refuses browsers.
	if code, _ := h.do(t, http.MethodGet, "/inbox", "", nil); code != http.StatusOK {
		t.Fatalf("control API: %d", code)
	}
	if code, _ := h.do(t, http.MethodGet, "/inbox", "", map[string]string{"Origin": h.srv.URL}); code != http.StatusForbidden {
		t.Fatalf("control API with Origin: %d", code)
	}
	// Restarting with changed settings works.
	changed := strings.Replace(validJSON(t), `"alice"`, `"carol"`, 1)
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", changed, h.tokenHdr()); code != http.StatusOK || strings.Contains(body, "error") {
		t.Fatalf("resave: %d %s", code, body)
	}
	if st := h.app.Status(); st.Node != "carol" || !st.Running {
		t.Fatalf("status after resave %+v", st)
	}
}

func TestSaveReturnsSanitizedNormalizedReactiveSlices(t *testing.T) {
	h := newHarness(t)
	h.app.mu.Lock()
	h.app.s.API = "127.0.0.1:7599"
	h.app.s.Secret = strings.Repeat("s", 32)
	h.app.s.HandlerCommand = []string{"private-agent.exe", "--private-argument"}
	h.app.mu.Unlock()

	body := `{"node":"  alice  ","code":"","handler":"none","api":""}`
	code, raw := h.do(t, http.MethodPost, "/ui/api/settings", body, h.tokenHdr())
	if code != http.StatusOK {
		t.Fatalf("save: %d %s", code, raw)
	}
	var got struct {
		Saved     bool               `json:"saved"`
		Settings  *settings.Settings `json:"settings"`
		Status    *Status            `json:"status"`
		Dashboard *DashboardSummary  `json:"dashboard"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Saved || got.Settings == nil || got.Status == nil || got.Dashboard == nil {
		t.Fatalf("reactive slices missing: %+v", got)
	}
	if got.Settings.Node != "alice" || got.Settings.API != "127.0.0.1:7599" {
		t.Fatalf("settings are not normalized/current: %+v", got.Settings)
	}
	if got.Settings.Secret != "" || len(got.Settings.HandlerCommand) != 0 || strings.Contains(raw, "private-agent") || strings.Contains(raw, strings.Repeat("s", 32)) {
		t.Fatalf("private settings leaked: %s", raw)
	}
	if got.Status.Node != "alice" || got.Dashboard.Status.Node != "alice" {
		t.Fatalf("status/dashboard do not describe saved state: status=%+v dashboard=%+v", got.Status, got.Dashboard)
	}
}

func TestQuitEndpoint(t *testing.T) {
	noQuit := newHarness(t)
	if code, _ := noQuit.do(t, http.MethodPost, "/ui/api/quit", "", noQuit.tokenHdr()); code != http.StatusNotImplemented {
		t.Fatalf("quit without QuitFunc: %d, want 501", code)
	}
	quits := make(chan struct{}, 2)
	h := newHarness(t, func(a *App) { a.QuitFunc = func() { quits <- struct{}{} } })
	if code, _ := h.do(t, http.MethodPost, "/ui/api/quit", "", nil); code != http.StatusForbidden {
		t.Fatalf("quit without token: %d", code)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/quit", "", h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("quit: %d %s", code, body)
	}
	select {
	case <-quits:
	case <-time.After(5 * time.Second):
		t.Fatal("QuitFunc not called")
	}
	if len(quits) != 0 {
		t.Fatal("QuitFunc called without a token")
	}
}

func TestSetAPIAddrPersists(t *testing.T) {
	h := newHarness(t)
	if err := h.app.SetAPIAddr("0.0.0.0:7520"); err == nil {
		t.Fatal("non-loopback api accepted")
	}
	if err := h.app.SetAPIAddr("127.0.0.1:7599"); err != nil {
		t.Fatal(err)
	}
	if code, body := h.do(t, http.MethodPost, "/ui/api/settings", validJSON(t), h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if s, _, err := settings.Load(h.path); err != nil || s.API != "127.0.0.1:7599" {
		t.Fatalf("persisted api %q err=%v", s.API, err)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	h := newHarness(t)
	bad := strings.Replace(validJSON(t), "abc123", "abc12", 1)
	code, body := h.do(t, http.MethodPost, "/ui/api/settings", bad, h.tokenHdr())
	if code != http.StatusBadRequest || !strings.Contains(body, uiStrings["error.code"]) {
		t.Fatalf("save: %d %s", code, body)
	}
	if h.app.Configured() {
		t.Fatal("invalid settings marked configured")
	}
}

// «Проекты» round-trip through the settings API: saved, announced as areas,
// served back, kept by a request without the field, cleared by {}, and a bad
// row is reported as one sentence.
func TestSettingsProjectsRoundTrip(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	dirJSON, _ := json.Marshal(dir)
	body := `{"node":"alice","code":"","handler":"none","areas":["dev"],"projects":{" site ":{"dir":` + string(dirJSON) + `,"write":true}}}`
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", body, h.tokenHdr()); code != http.StatusOK || strings.Contains(raw, `"error"`) {
		t.Fatalf("save: %d %s", code, raw)
	}
	_, raw := h.do(t, http.MethodGet, "/ui/api/settings", "", h.tokenHdr())
	var got settings.Settings
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if p := got.Projects["site"]; p.Dir != dir || len(got.Projects) != 1 {
		t.Fatalf("projects served back %+v", got.Projects)
	}
	if strings.Join(got.Areas, ",") != "dev,site" {
		t.Fatalf("project area not declared: areas %v", got.Areas)
	}
	if d := h.app.Settings().Project("site"); d != dir {
		t.Fatalf("running settings Project(site) = %q", d)
	}
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", `{"node":"alice","handler":"none"}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("save without projects: %d %s", code, raw)
	}
	if s, _, _ := settings.Load(h.path); len(s.Projects) != 1 {
		t.Fatalf("a request without projects dropped them: %+v", s.Projects)
	}
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", `{"node":"alice","handler":"none","projects":{}}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("clear projects: %d %s", code, raw)
	}
	if s, _, _ := settings.Load(h.path); len(s.Projects) != 0 {
		t.Fatalf("projects not cleared: %+v", s.Projects)
	}
	for _, bad := range []string{`{"":{"dir":` + string(dirJSON) + `}}`, `{"site":{"dir":""}}`, `{"site":{"dir":"relative"}}`} {
		code, raw := h.do(t, http.MethodPost, "/ui/api/settings", `{"node":"alice","handler":"none","projects":`+bad+`}`, h.tokenHdr())
		if code != http.StatusBadRequest || !strings.Contains(raw, uiStrings["error.projects"]) {
			t.Errorf("projects %s: %d %s", bad, code, raw)
		}
	}
}

// The page's first save carries only a name: it must succeed. Without a code
// the node waits for one; with a code and no peer it runs and waits to be dialed.
func TestPartialSave(t *testing.T) {
	h := newHarness(t)
	name := `{"node":"morgott","code":"","peer_addr":"","handler":"none","work_dir":"","listen":"","api":"","areas":[],"peer_name":"","autostart":false}`
	code, body := h.do(t, http.MethodPost, "/ui/api/settings", name, h.tokenHdr())
	if code != http.StatusOK || !strings.Contains(body, `"saved":true`) || strings.Contains(body, "error") {
		t.Fatalf("name-only save: %d %s", code, body)
	}
	if st := h.app.Status(); !st.Configured || st.Running || st.Problem != "link.no_code" || st.Error != "" {
		t.Fatalf("status after name-only save %+v", st)
	}
	withCode := `{"node":"morgott","code":"k7Q2mX","listen":"127.0.0.1:0","handler":"none"}`
	code, body = h.do(t, http.MethodPost, "/ui/api/settings", withCode, h.tokenHdr())
	if code != http.StatusOK || strings.Contains(body, "error") {
		t.Fatalf("name+code save: %d %s", code, body)
	}
	if st := h.app.Status(); !st.Running || st.Problem != "link.no_peer" || st.Error != "" {
		t.Fatalf("status after name+code save %+v", st)
	}
	if s, _, _ := settings.Load(h.path); s.Code != "K7Q2MX" {
		t.Fatalf("code stored as %q, want upper case", s.Code)
	}
}

// Every settings error reaches the page as one Russian sentence, never a Go error.
func TestSaveErrorsAreSentences(t *testing.T) {
	h := newHarness(t)
	cases := map[string]string{
		`{"node":""}`:                               "error.node",
		`{"node":"a b"}`:                            "error.node",
		`{"node":"a","code":"12345!"}`:              "error.code",
		`{"node":"a","peer_addr":"10.0.0.1:99999"}`: "error.peer_addr",
		`{"node":"a","code":"K7Q2-MXPA-4RTB","handler":"claude","work_dir":""}`: "error.work_dir",
		`{"node":"a","api":"0.0.0.0:7520"}`:                                     "error.api",
		`not json`:                                                              "error.bad_request",
	}
	for body, key := range cases {
		code, got := h.do(t, http.MethodPost, "/ui/api/settings", body, h.tokenHdr())
		var r struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(got), &r); err != nil || code != http.StatusBadRequest || r.Error != uiStrings[key] {
			t.Errorf("%s: %d %s, want %q", body, code, got, uiStrings[key])
		}
	}
	// A bind failure is saved but explained.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	busy := `{"node":"a","code":"ABC123","listen":"` + ln.Addr().String() + `"}`
	_, got := h.do(t, http.MethodPost, "/ui/api/settings", busy, h.tokenHdr())
	want := msg("error.listen", map[string]string{"addr": ln.Addr().String()})
	if !strings.Contains(got, `"saved":true`) || !strings.Contains(got, want) || h.app.Status().Error != want {
		t.Fatalf("busy listen: %s, want %q", got, want)
	}
}

// A config written before pairing codes (long secret, peer name) still runs.
func TestLegacySecretConfigRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"node":"alice","listen":"127.0.0.1:0","peer_name":"bob","peer_addr":"127.0.0.1:1",` +
		`"secret":"` + strings.Repeat("s", 40) + `","areas":[],"handler":"none","work_dir":"","autostart":false}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := a.Status(); !st.Running || st.Peer != "bob" || st.Problem != "" {
		t.Fatalf("legacy status %+v", st)
	}
}

// A legacy 6-character code draws a warning only while the listener is
// reachable beyond private networks; a current code never does.
func TestWeakCodeWarning(t *testing.T) {
	for _, c := range []struct {
		code, listen, want string
	}{
		{"abc123", "", "link.weak_code"},
		{"abc123", "203.0.113.5", "link.weak_code"},
		{"abc123", "10.147.20.5", ""},
		{"abc123", "127.0.0.1:7420", ""},
		{"K7Q2-MXAB-CDEF", "", ""},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		s := settings.Settings{Node: "alice", Code: c.code, Listen: c.listen, Handler: worker.HandlerNone}
		if err := settings.Save(path, s); err != nil {
			t.Fatal(err)
		}
		a, err := New(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		st := a.Status()
		if st.Warning != c.want {
			t.Errorf("code %q listen %q: warning %q, want %q", c.code, c.listen, st.Warning, c.want)
		}
		if c.want != "" && !strings.Contains(st.Summary(), uiStrings["tray.weak_code"]) {
			t.Errorf("tray summary %q has no warning", st.Summary())
		}
	}
}
