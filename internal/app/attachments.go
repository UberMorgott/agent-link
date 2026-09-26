package app

import (
	"errors"
	"net/http"

	"github.com/UberMorgott/agent-link/internal/node"
)

// Chat attachments in the web UI:
//
//	POST /ui/api/projects/{pid}/attachments   the file as the raw body (?name=) or multipart -> node.Attachment
//	GET  /ui/files/{pid}/{id}?k=&name=&download=1 the file: images inline, anything else as a download
//
// The file route carries no token header, so <img> and download links work.
// Instead it needs the blob's capability k (node.Attachment.Key): an HMAC under
// the node's secret that the token-protected API hands out with the upload and
// the chat messages; knowing the content (its sha256) is not enough. Like every
// route it answers only a loopback Host (loopbackHost), and a cross-site
// request (Sec-Fetch-Site, Origin) is refused.
func (a *App) attachmentRoutes(api, ui *http.ServeMux) {
	api.HandleFunc("POST /ui/api/projects/{pid}/attachments", func(w http.ResponseWriter, r *http.Request) {
		n, ok := a.contextNode(w, r.PathValue("pid"))
		if !ok {
			return
		}
		att, err := n.Upload(w, r)
		if err != nil {
			if !attachmentFailed(w, err) {
				writeCodedError(w, http.StatusBadRequest, "attachment")
			}
			return
		}
		writeJSON(w, att)
	})
	ui.HandleFunc("GET /ui/files/{pid}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host) ||
			(r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin" && r.Header.Get("Sec-Fetch-Site") != "none") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		n, ok := a.contextNode(w, r.PathValue("pid"))
		if !ok {
			return
		}
		id := r.PathValue("id")
		if !n.AttachmentKeyOK(id, r.URL.Query().Get("k")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		n.ServeAttachment(w, r, id)
	})
}

// attachmentFailed answers an attachment error with its code; false for any
// other error.
func attachmentFailed(w http.ResponseWriter, err error) bool {
	switch {
	case !errors.Is(err, node.ErrAttachment):
		return false
	case errors.Is(err, node.ErrAttachmentType):
		writeCodedError(w, http.StatusBadRequest, "attachment_type")
	case errors.Is(err, node.ErrAttachmentSize):
		writeCodedError(w, http.StatusBadRequest, "attachment_size")
	default:
		writeCodedError(w, http.StatusBadRequest, "attachment")
	}
	return true
}
