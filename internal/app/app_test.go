package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
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
		Secret: strings.Repeat("k", 32), Areas: []string{"dev"}, Handler: worker.HandlerNone, Autostart: true,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPageEmbedsToken(t *testing.T) {
	h := newHarness(t)
	for _, page := range []string{"/ui/settings", "/ui/inbox"} {
		code, body := h.do(t, http.MethodGet, page, "", nil)
		if code != http.StatusOK || !strings.Contains(body, h.app.token) {
			t.Fatalf("%s: status %d, token embedded=%v", page, code, strings.Contains(body, h.app.token))
		}
	}
	if code, _ := h.do(t, http.MethodGet, "/ui/static/settings.js", "", nil); code != http.StatusOK {
		t.Fatalf("static: %d", code)
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

func TestRebindingHostRejected(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/ui/settings", nil)
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
	if err != nil || !ok || s.Node != "alice" || len(s.Secret) != 32 {
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
	bad := strings.Replace(validJSON(t), strings.Repeat("k", 32), "short", 1)
	code, body := h.do(t, http.MethodPost, "/ui/api/settings", bad, h.tokenHdr())
	if code != http.StatusBadRequest || !strings.Contains(body, "secret") {
		t.Fatalf("save: %d %s", code, body)
	}
	if h.app.Configured() {
		t.Fatal("invalid settings marked configured")
	}
}
