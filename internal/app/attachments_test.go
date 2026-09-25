package app

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// The web UI uploads a pasted image, gets its description back and shows it
// from /ui/files without the token; a cross-site page cannot load it, and a
// file of a type outside the allowlist is refused with its code.
func TestAttachmentUploadAndServe(t *testing.T) {
	h := projectsHarness(t, "alice", "")
	var p ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Сайт", "dir": t.TempDir()}, &p); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	img := buf.Bytes()

	code, raw := h.do(t, http.MethodPost, "/ui/api/projects/"+p.ID+"/attachments?name="+url.QueryEscape("../вставка.png"), string(img), h.tokenHdr())
	var att node.Attachment
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &att) != nil || att.MIME != node.MIMEPNG || att.Name != "вставка.png" || att.Size != int64(len(img)) {
		t.Fatalf("upload: %d %s", code, raw)
	}
	// The same file as a multipart form (the file picker).
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("file", "pick.png")
	_, _ = fw.Write(img)
	_ = mw.Close()
	hdr := h.tokenHdr()
	hdr["Content-Type"] = mw.FormDataContentType()
	var picked node.Attachment
	if code, raw := h.do(t, http.MethodPost, "/ui/api/projects/"+p.ID+"/attachments", form.String(), hdr); code != http.StatusOK ||
		json.Unmarshal([]byte(raw), &picked) != nil || picked.ID != att.ID || picked.Name != "pick.png" {
		t.Fatalf("multipart upload: %d %s", code, raw)
	}
	for body, want := range map[string]string{"MZ\x90\x00\x03": "attachment_type", "": "attachment"} {
		code, raw := h.do(t, http.MethodPost, "/ui/api/projects/"+p.ID+"/attachments?name=x.png", body, h.tokenHdr())
		var e apiError
		if code != http.StatusBadRequest || json.Unmarshal([]byte(raw), &e) != nil || e.Code != want || e.Error != uiStrings["error."+want] {
			t.Errorf("upload %q: %d %s, want %s", body, code, raw, want)
		}
	}
	if code, _ := h.do(t, http.MethodPost, "/ui/api/projects/"+p.ID+"/attachments?name=x.png", string(img), nil); code != http.StatusForbidden {
		t.Fatalf("upload without token: %d", code)
	}

	// get answers status, headers and body of a GET without the token.
	get := func(path string, hdr map[string]string) (int, http.Header, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, data
	}
	code, head, data := get("/ui/files/"+p.ID+"/"+att.ID+"?name=a.png", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if code != http.StatusOK || head.Get("Content-Type") != node.MIMEPNG || !bytes.Equal(data, img) ||
		head.Get("Content-Disposition") != `inline; filename=a.png` {
		t.Fatalf("serve: %d %v", code, head)
	}
	if _, head, _ := get("/ui/files/"+p.ID+"/"+att.ID+"?name=a.png&download=1", nil); head.Get("Content-Disposition") != `attachment; filename=a.png` {
		t.Fatalf("download: %v", head)
	}
	if code, _, _ := get("/ui/files/"+p.ID+"/"+att.ID, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", code)
	}
	if code, _, _ := get("/ui/files/"+p.ID+"/"+att.ID, map[string]string{"Origin": "http://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", code)
	}
	for _, id := range []string{"0000000000000000000000000000000000000000000000000000000000000000", "..%2F..%2Fconfig.json"} {
		if code, _, _ := get("/ui/files/"+p.ID+"/"+id, nil); code != http.StatusNotFound {
			t.Fatalf("id %s: %d", id, code)
		}
	}
}
