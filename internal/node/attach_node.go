package node

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
)

// PutAttachment stores an uploaded file (POST /attachments) and describes it.
func (n *Node) PutAttachment(r io.Reader, name string) (Attachment, error) {
	return n.atts.put(r, name)
}

// AttachmentFile returns the stored blob id as a file path and its
// description (name "file"); ErrAttachment when it is not here.
func (n *Node) AttachmentFile(id string) (string, Attachment, error) {
	a, err := n.atts.describe(id, "file")
	if err != nil {
		return "", Attachment{}, err
	}
	return n.atts.path(id), a, nil
}

// resolveAttachments turns a send request's uploaded attachments (id and name)
// and local files into the message's attachments, within the limits.
func (n *Node) resolveAttachments(req SendRequest) ([]Attachment, error) {
	if len(req.Attachments)+len(req.Files) > MaxAttachments {
		return nil, fmt.Errorf("%w: at most %d attachments: %w", ErrAttachment, MaxAttachments, ErrAttachmentSize)
	}
	var out []Attachment
	var total int64
	add := func(a Attachment) error {
		total += a.Size
		if total > MaxAttachmentTotal {
			return fmt.Errorf("%w: over %d MB in all: %w", ErrAttachment, MaxAttachmentTotal>>20, ErrAttachmentSize)
		}
		out = append(out, a)
		return nil
	}
	for _, u := range req.Attachments {
		a, err := n.atts.describe(u.ID, u.Name)
		if err != nil {
			return nil, err
		}
		if err := add(a); err != nil {
			return nil, err
		}
	}
	for _, p := range req.Files {
		path, err := n.allowedFile(p, req.Folder)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(path) //nolint:gosec // G304: checked by allowedFile
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAttachment, err)
		}
		a, err := n.atts.put(f, filepath.Base(path))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if err := add(a); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// allowedFile resolves a local file an agent attaches: an absolute path (or
// one relative to folder), links resolved, inside this node's working or
// project folder or the OS temp folder; for a node without folders, the
// caller's folder counts as its working folder.
func (n *Node) allowedFile(p, folder string) (string, error) {
	if !filepath.IsAbs(p) {
		if folder == "" || !filepath.IsAbs(folder) {
			return "", fmt.Errorf("%w: %s: not an absolute path", ErrAttachment, p)
		}
		p = filepath.Join(folder, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAttachment, err)
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAttachment, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s: not a file", ErrAttachment, p)
	}
	roots := []string{os.TempDir()}
	if n.folders.work != "" {
		roots = append(roots, n.folders.work)
	} else if n.cfg.Project == "" && folder != "" {
		roots = append(roots, folder)
	}
	for _, dir := range n.folders.projects {
		roots = append(roots, dir)
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if inFolder(root, real) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%w: %s: %w", ErrAttachment, p, ErrAttachmentPath)
}

// cleanAttachments checks the attachments of a received message: valid ids,
// safe names, at most MaxAttachments, the local fields cleared, and the type
// and size of each blob this node has.
func (n *Node) cleanAttachments(m *Message) {
	if len(m.Attachments) == 0 {
		m.Attachments = nil
		return
	}
	var out []Attachment
	for _, a := range m.Attachments {
		if len(out) == MaxAttachments {
			break
		}
		if !validSHA(a.ID) {
			continue
		}
		a.Name, a.Failed, a.Path = SanitizeName(a.Name), false, ""
		if d, err := n.atts.describe(a.ID, a.Name); err == nil {
			a.MIME, a.Size = d.MIME, d.Size
		} else if a.Size < 0 || a.Size > MaxAttachmentSize || !knownMIME(a.MIME) {
			a.Size, a.MIME = 0, ""
		}
		out = append(out, a)
	}
	m.Attachments = out
}

func knownMIME(t string) bool {
	switch t {
	case MIMEPNG, MIMEJPEG, MIMEGIF, MIMEWebP, MIMEPDF, MIMEText:
		return true
	}
	return false
}

// withState returns a copy of atts with Failed set for the blobs not here.
func (n *Node) withState(atts []Attachment) []Attachment {
	if len(atts) == 0 {
		return atts
	}
	out := make([]Attachment, len(atts))
	for i, a := range atts {
		a.Failed, a.Path = !n.atts.has(a.ID), ""
		out[i] = a
	}
	return out
}

// materialize copies m's attachments into the workspace of area (its project
// folder here, else the working folder; the store itself when there is none)
// and sets their Path.
func (n *Node) materialize(m *Message, area string) {
	if len(m.Attachments) == 0 {
		return
	}
	dir := n.folders.projects[n.localArea(area)]
	if dir == "" {
		dir = n.folders.work
	}
	atts := n.withState(m.Attachments)
	for i, a := range atts {
		if a.Failed {
			continue
		}
		if dir == "" {
			atts[i].Path = n.atts.path(a.ID)
			continue
		}
		p, err := n.atts.materialize(a, dir)
		if err != nil {
			n.log.Warn("materialize attachment", "id", a.ID, "err", err)
			atts[i].Path = n.atts.path(a.ID)
			continue
		}
		atts[i].Path = p
	}
	m.Attachments = atts
}

// pushAttachments sends peer the blobs of m it has not been sent on this
// session (pushed), before m itself.
func (n *Node) pushAttachments(pc *peerConn, m *Message, pushed map[string]bool) error {
	if len(m.Attachments) == 0 || !pc.has(CapAttachments) {
		return nil
	}
	for _, a := range m.Attachments {
		if pushed[a.ID] {
			continue
		}
		if !n.atts.has(a.ID) {
			n.log.Warn("attachment blob missing, not sent", "id", a.ID, "peer", pc.peer)
			pushed[a.ID] = true
			continue
		}
		err := n.atts.chunks(a.ID, func(c attChunk) error { return pc.write(frame{Type: frameAtt, Att: &c}) })
		if err != nil {
			return err
		}
		pushed[a.ID] = true
	}
	return nil
}

// receiveAttachment takes an att frame from peer.
func (n *Node) receiveAttachment(peer string, c *attChunk) {
	if c == nil {
		return
	}
	done, err := n.atts.receive(peer, *c)
	switch {
	case err != nil:
		n.log.Warn("attachment transfer dropped", "peer", peer, "err", err)
	case done:
		n.log.Info("attachment received", "peer", peer, "id", c.ID, "size", c.Size)
		n.changed("messages")
	}
}

// AttachmentRoutes mounts POST prefix/attachments (a raw body with ?name=, or
// multipart form-data with one file) -> Attachment, and GET
// prefix/attachments/{id}?name=&download=1 -> the file.
func (n *Node) AttachmentRoutes(mux *http.ServeMux, prefix string, fail func(w http.ResponseWriter, code int, err error)) {
	mux.HandleFunc("POST "+prefix+"/attachments", func(w http.ResponseWriter, r *http.Request) {
		a, err := n.Upload(w, r)
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		writeJSONResponse(w, a)
	})
	mux.HandleFunc("GET "+prefix+"/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		n.ServeAttachment(w, r, r.PathValue("id"))
	})
}

// Upload stores the one file of an upload request: the raw body named by
// ?name=, or the first file of a multipart form.
func (n *Node) Upload(w http.ResponseWriter, r *http.Request) (Attachment, error) {
	body := http.MaxBytesReader(w, r.Body, MaxAttachmentSize+1<<20)
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		return n.PutAttachment(body, r.URL.Query().Get("name"))
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return Attachment{}, fmt.Errorf("%w: no file in the form", ErrAttachment)
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		a, err := n.PutAttachment(part, part.FileName())
		_ = part.Close()
		return a, err
	}
}

// ServeAttachment answers blob id as a file: images inline, anything else as a
// download, named by ?name= (sanitized).
func (n *Node) ServeAttachment(w http.ResponseWriter, r *http.Request, id string) {
	path, a, err := n.AttachmentFile(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := SanitizeName(r.URL.Query().Get("name"))
	disp := "attachment"
	if a.IsImage() && r.URL.Query().Get("download") != "1" {
		disp = "inline"
	}
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}
