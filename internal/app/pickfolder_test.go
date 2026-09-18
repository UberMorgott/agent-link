package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func pickHarness(t *testing.T, pick func(start, title string) (string, error)) *harness {
	t.Helper()
	return newHarness(t, func(a *App) { a.PickFolder = pick })
}

func decodePick(t *testing.T, body string) pickResult {
	t.Helper()
	var r pickResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return r
}

func TestPickFolderRequiresTokenAndSameOrigin(t *testing.T) {
	called := false
	h := pickHarness(t, func(string, string) (string, error) { called = true; return `C:\x`, nil })
	cases := map[string]map[string]string{
		"no token":     nil,
		"wrong token":  {TokenHeader: "nope"},
		"cross origin": {TokenHeader: h.app.token, "Origin": "http://evil.example"},
		"cross site":   {TokenHeader: h.app.token, "Sec-Fetch-Site": "cross-site"},
	}
	for name, hdr := range cases {
		if code, _ := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{}`, hdr); code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, code)
		}
	}
	if code, _ := h.do(t, http.MethodGet, "/ui/api/pick-folder", "", h.tokenHdr()); code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", code)
	}
	if called {
		t.Fatal("dialog opened for a rejected request")
	}
}

func TestPickFolderReturnsChosenPath(t *testing.T) {
	var gotStart, gotTitle string
	h := pickHarness(t, func(start, title string) (string, error) {
		gotStart, gotTitle = start, title
		return `E:\DEV\CodeDungeon`, nil
	})
	code, body := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{"start":" E:\\DEV "}`, h.tokenHdr())
	if code != http.StatusOK || decodePick(t, body).Path != `E:\DEV\CodeDungeon` {
		t.Fatalf("pick: %d %s", code, body)
	}
	if gotStart != `E:\DEV` || gotTitle != msg("settings.work_dir.pick_title", nil) {
		t.Fatalf("start %q title %q", gotStart, gotTitle)
	}
}

func TestPickFolderCancel(t *testing.T) {
	h := pickHarness(t, func(string, string) (string, error) { return "", ErrPickCancelled })
	code, body := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{"start":""}`, h.tokenHdr())
	r := decodePick(t, body)
	if code != http.StatusOK || !r.Cancelled || r.Path != "" || r.Message != msg("settings.work_dir.cancelled", nil) {
		t.Fatalf("cancel: %d %s", code, body)
	}
}

func TestPickFolderFailures(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		key  string
	}{
		{ErrPickUnsupported, http.StatusNotImplemented, "error.pick_unsupported"},
		{errors.New("COM broke"), http.StatusInternalServerError, "error.pick"},
	} {
		h := pickHarness(t, func(string, string) (string, error) { return "", c.err })
		code, body := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{}`, h.tokenHdr())
		var e map[string]string
		_ = json.Unmarshal([]byte(body), &e)
		if code != c.code || e["error"] != msg(c.key, nil) {
			t.Errorf("%v: %d %s", c.err, code, body)
		}
	}
}

func TestPickFolderOneDialogAtATime(t *testing.T) {
	opened := make(chan struct{})
	release := make(chan struct{})
	h := pickHarness(t, func(string, string) (string, error) {
		opened <- struct{}{}
		<-release
		return `C:\first`, nil
	})
	type resp struct {
		code int
		body string
	}
	first := make(chan resp, 1)
	go func() {
		code, body := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{}`, h.tokenHdr())
		first <- resp{code, body}
	}()
	<-opened
	code, body := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{}`, h.tokenHdr())
	var e map[string]string
	_ = json.Unmarshal([]byte(body), &e)
	if code != http.StatusConflict || e["error"] != msg("error.pick_busy", nil) {
		t.Fatalf("second click: %d %s", code, body)
	}
	close(release)
	if r := <-first; r.code != http.StatusOK || decodePick(t, r.body).Path != `C:\first` {
		t.Fatalf("first click: %d %s", r.code, r.body)
	}
	// The guard is released once the dialog closes.
	go func() { <-opened }()
	if code, body := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("after close: %d %s", code, body)
	}
}
