package node

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Attachments (files and images on chat messages). A blob is stored once per
// node, content-addressed: <data dir>/attachments/<sha256 hex>. A message
// carries only their descriptions (Message.Attachments); the blobs travel
// before it on the same session, in att frames (CapAttachments), so the
// receiver has them when the message arrives. The receiver checks size, hash
// and type of every blob; one that does not arrive or fails a check leaves its
// attachment failed (Attachment.Failed). An older peer ignores both the field
// and the frames and reads the text fallback lines of the body.
//
// Only types both Claude Code and Codex read are accepted, by sniffed content,
// never by extension: PNG, JPEG, GIF, WebP images, PDF, and UTF-8 text.
//
// For agents a message's attachments are copied into the workspace
// (<folder>/.agentlink/attachments/<sha8>-<name>, git-ignored) and listed by
// absolute path (Attachment.Path) in unread messages and wake prompts.

// CapAttachments: sends and reads att frames and Message.Attachments.
const CapAttachments = "attachments-v1"

// frameAtt carries one chunk of an attachment blob (frame.Att).
const frameAtt = "att"

// Attachment limits.
const (
	MaxAttachmentSize  = 10 << 20 // bytes of one attachment
	MaxAttachments     = 10       // attachments on one message
	MaxAttachmentTotal = 50 << 20 // bytes of all attachments on one message
	// attChunkSize is the payload of one att frame; base64 in JSON it stays
	// well under maxFrame.
	attChunkSize = 256 << 10
	// maxNameLen bounds an attachment's file name, in bytes.
	maxNameLen = 120
	// attachDir is the folder of materialized attachments in a workspace.
	attachDir = ".agentlink"
)

// Attachment types (Attachment.MIME).
const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
	MIMEGIF  = "image/gif"
	MIMEWebP = "image/webp"
	MIMEPDF  = "application/pdf"
	MIMEText = "text/plain; charset=utf-8"
)

var (
	// ErrAttachment wraps every refused attachment.
	ErrAttachment = errors.New("attachment refused")
	// ErrAttachmentType: the content is none of the allowed types.
	ErrAttachmentType = errors.New("type not allowed (images png/jpeg/gif/webp, pdf, utf-8 text)")
	// ErrAttachmentSize: over a size or count limit.
	ErrAttachmentSize = errors.New("too large")
	// ErrAttachmentPath: a local file outside the allowed folders.
	ErrAttachmentPath = errors.New("file is outside the project folder and the temp folder")
)

// Attachment describes one file on a message. ID is the sha256 of the
// content (hex), Name a sanitized base name, MIME the sniffed type.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
	// Failed: this node does not have the blob (it did not arrive, or failed
	// the checks). Set locally, never sent.
	Failed bool `json:"failed,omitempty"`
	// Path is the absolute path of the copy in the agent's workspace, on
	// unread messages. Set locally, never sent.
	Path string `json:"path,omitempty"`
}

// IsImage reports whether a is an image.
func (a Attachment) IsImage() bool { return strings.HasPrefix(a.MIME, "image/") }

// attChunk is one piece of a blob on the wire: Data is bytes [Off,
// Off+len(Data)) of the blob ID of Size bytes. Chunks of one blob come in
// order, from 0.
type attChunk struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
	Off  int64  `json:"off"`
	Data []byte `json:"data"`
}

// validSHA reports whether id is a sha256 hex digest (lower case).
func validSHA(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// FallbackLine is the body line that stands for attachment a on peers
// without CapAttachments: «[attachment: name]».
func FallbackLine(a Attachment) string { return "[attachment: " + a.Name + "]" }

// withFallback appends one fallback line per attachment to body.
func withFallback(body string, atts []Attachment) string {
	lines := make([]string, 0, len(atts))
	for _, a := range atts {
		lines = append(lines, FallbackLine(a))
	}
	if len(lines) == 0 {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return strings.Join(lines, "\n")
	}
	return strings.TrimRight(body, "\n") + "\n" + strings.Join(lines, "\n")
}

// BodyText is body without the trailing fallback lines of atts: the text the
// author wrote, for a reader that shows the attachments themselves.
func BodyText(body string, atts []Attachment) string {
	if len(atts) == 0 {
		return body
	}
	lines := strings.Split(body, "\n")
	for i := len(atts) - 1; i >= 0 && len(lines) > 0; i-- {
		if lines[len(lines)-1] != FallbackLine(atts[i]) {
			break
		}
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// windowsReserved are device names Windows refuses as file names.
var windowsReserved = []string{"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

// SanitizeName turns any name into a safe base name for every OS: no
// directories, separators, control or reserved characters, no leading dots,
// no reserved device name, at most maxNameLen bytes; "file" when nothing is left.
func SanitizeName(name string) string {
	// The last element of either separator style.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), strings.ContainsRune(`<>:"|?*/\`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	name = strings.TrimRight(name, ". ")
	for len(name) > maxNameLen {
		ext := filepath.Ext(name)
		if len(ext) > 16 || len(ext) >= maxNameLen {
			ext = ""
		}
		stem := name[:len(name)-len(ext)]
		cut := maxNameLen - len(ext)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = stem[:cut] + ext
	}
	if name == "" {
		return "file"
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	if slices.Contains(windowsReserved, stem) {
		name = "_" + name
	}
	return name
}

// sniff returns the allowed type of the content read by r (at most
// MaxAttachmentSize bytes), or ErrAttachmentType.
func sniff(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxAttachmentSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxAttachmentSize {
		return "", ErrAttachmentSize
	}
	return sniffBytes(data)
}

// sniffBytes classifies data: an allowed image or PDF by its magic, else UTF-8
// text without NUL or other binary control characters.
func sniffBytes(data []byte) (string, error) {
	switch ct := http.DetectContentType(data); ct {
	case MIMEPNG, MIMEJPEG, MIMEGIF, MIMEWebP, MIMEPDF:
		return ct, nil
	}
	if len(data) == 0 || !utf8.Valid(data) {
		return "", ErrAttachmentType
	}
	for _, c := range data {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != 0x1b {
			return "", ErrAttachmentType
		}
	}
	return MIMEText, nil
}

// attachStore keeps the blobs of one node and the blobs being received.
type attachStore struct {
	dir string

	mu    sync.Mutex
	parts map[string]*partial // peer + "/" + id
}

// partial is a blob being received from one peer.
type partial struct {
	f    *os.File
	h    hash.Hash
	size int64
	got  int64
}

func openAttachStore(dataDir string) (*attachStore, error) {
	dir := filepath.Join(dataDir, "attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Leftovers of uploads and transfers a stop cut short.
	for _, pat := range []string{"up-*", "part-*"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, p := range m {
			_ = os.Remove(p)
		}
	}
	return &attachStore{dir: dir, parts: map[string]*partial{}}, nil
}

// path is where blob id is stored; id must be valid.
func (s *attachStore) path(id string) string { return filepath.Join(s.dir, id) }

// has reports whether blob id is stored.
func (s *attachStore) has(id string) bool {
	if !validSHA(id) {
		return false
	}
	st, err := os.Stat(s.path(id))
	return err == nil && st.Mode().IsRegular()
}

// describe returns the attachment of stored blob id named name.
func (s *attachStore) describe(id, name string) (Attachment, error) {
	if !s.has(id) {
		return Attachment{}, fmt.Errorf("%w: unknown attachment %s", ErrAttachment, id)
	}
	f, err := os.Open(s.path(id))
	if err != nil {
		return Attachment{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return Attachment{}, err
	}
	mime, err := sniff(f)
	if err != nil {
		return Attachment{}, fmt.Errorf("%w: %s: %w", ErrAttachment, name, err)
	}
	return Attachment{ID: id, Name: SanitizeName(name), MIME: mime, Size: st.Size()}, nil
}

// put stores the content read from r as an attachment named name: at most
// MaxAttachmentSize bytes of an allowed type.
func (s *attachStore) put(r io.Reader, name string) (Attachment, error) {
	name = SanitizeName(name)
	f, err := os.CreateTemp(s.dir, "up-*")
	if err != nil {
		return Attachment{}, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, MaxAttachmentSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return Attachment{}, err
	case n > MaxAttachmentSize:
		return Attachment{}, fmt.Errorf("%w: %s: %w (over %d MB)", ErrAttachment, name, ErrAttachmentSize, MaxAttachmentSize>>20)
	case n == 0:
		return Attachment{}, fmt.Errorf("%w: %s: empty file", ErrAttachment, name)
	}
	data, err := os.ReadFile(tmp) //nolint:gosec // G304: our own temp file
	if err != nil {
		return Attachment{}, err
	}
	mime, err := sniffBytes(data)
	if err != nil {
		return Attachment{}, fmt.Errorf("%w: %s: %w", ErrAttachment, name, err)
	}
	id := hex.EncodeToString(h.Sum(nil))
	if err := s.commit(tmp, id); err != nil {
		return Attachment{}, err
	}
	return Attachment{ID: id, Name: name, MIME: mime, Size: n}, nil
}

// commit moves the verified file tmp to blob id, unless it is there already.
func (s *attachStore) commit(tmp, id string) error {
	if s.has(id) {
		return nil
	}
	return os.Rename(tmp, s.path(id))
}

// receive takes one chunk of a blob from peer. It reports done when the blob
// is complete and stored. A chunk out of order, a blob over the limits, a
// hash mismatch or a type not allowed drop the transfer with an error; the
// attachment then stays failed on the message.
func (s *attachStore) receive(peer string, c attChunk) (done bool, err error) {
	if !validSHA(c.ID) || c.Size <= 0 || c.Size > MaxAttachmentSize || c.Off < 0 ||
		len(c.Data) == 0 || len(c.Data) > attChunkSize || c.Off+int64(len(c.Data)) > c.Size {
		s.drop(peer, c.ID)
		return false, fmt.Errorf("%w: bad chunk of %.12s", ErrAttachment, c.ID)
	}
	if s.has(c.ID) {
		s.drop(peer, c.ID) // already here: the rest is ignored
		return false, nil
	}
	key := peer + "/" + c.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.parts[key]
	if c.Off == 0 {
		if p != nil {
			p.discard()
			delete(s.parts, key)
		}
		if s.countLocked(peer) >= MaxAttachments {
			return false, fmt.Errorf("%w: too many transfers from %s", ErrAttachment, peer)
		}
		f, err := os.CreateTemp(s.dir, "part-*")
		if err != nil {
			return false, err
		}
		p = &partial{f: f, h: sha256.New(), size: c.Size}
		s.parts[key] = p
	}
	if p == nil || p.got != c.Off || p.size != c.Size {
		if p != nil {
			p.discard()
			delete(s.parts, key)
		}
		return false, fmt.Errorf("%w: chunk of %.12s out of order", ErrAttachment, c.ID)
	}
	if _, err := p.f.Write(c.Data); err != nil {
		p.discard()
		delete(s.parts, key)
		return false, err
	}
	p.h.Write(c.Data)
	p.got += int64(len(c.Data))
	if p.got < p.size {
		return false, nil
	}
	delete(s.parts, key)
	tmp := p.f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := p.f.Close(); err != nil {
		return false, err
	}
	if hex.EncodeToString(p.h.Sum(nil)) != c.ID {
		return false, fmt.Errorf("%w: %.12s: hash mismatch", ErrAttachment, c.ID)
	}
	data, err := os.ReadFile(tmp) //nolint:gosec // G304: our own temp file
	if err != nil {
		return false, err
	}
	if _, err := sniffBytes(data); err != nil {
		return false, fmt.Errorf("%w: %.12s: %w", ErrAttachment, c.ID, err)
	}
	if err := s.commit(tmp, c.ID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *attachStore) countLocked(peer string) int {
	n := 0
	for k := range s.parts {
		if strings.HasPrefix(k, peer+"/") {
			n++
		}
	}
	return n
}

// drop abandons the transfer of id from peer, if any.
func (s *attachStore) drop(peer, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.parts[peer+"/"+id]; p != nil {
		p.discard()
		delete(s.parts, peer+"/"+id)
	}
}

// dropPeer abandons every transfer from peer (its session ended).
func (s *attachStore) dropPeer(peer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.parts {
		if strings.HasPrefix(k, peer+"/") {
			p.discard()
			delete(s.parts, k)
		}
	}
}

func (p *partial) discard() {
	_ = p.f.Close()
	_ = os.Remove(p.f.Name())
}

// chunks calls send with every chunk of blob id, in order.
func (s *attachStore) chunks(id string, send func(attChunk) error) error {
	f, err := os.Open(s.path(id))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	buf := make([]byte, attChunkSize)
	for off := int64(0); off < st.Size(); {
		n, err := io.ReadFull(f, buf)
		if n == 0 {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if err := send(attChunk{ID: id, Size: st.Size(), Off: off, Data: buf[:n]}); err != nil {
			return err
		}
		off += int64(n)
	}
	return nil
}

// materialize copies blob a into dir/.agentlink/attachments/<sha8>-<name>
// (once) and returns that absolute path. The .agentlink folder gets a
// .gitignore that ignores everything in it.
func (s *attachStore) materialize(a Attachment, dir string) (string, error) {
	if !s.has(a.ID) {
		return "", fmt.Errorf("%w: %.12s is not here", ErrAttachment, a.ID)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	base := filepath.Join(root, attachDir)
	out := filepath.Join(base, "attachments")
	if err := os.MkdirAll(out, 0o750); err != nil {
		return "", err
	}
	ignore := filepath.Join(base, ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("*\n"), 0o600); err != nil {
			return "", err
		}
	}
	target := filepath.Join(out, a.ID[:8]+"-"+SanitizeName(a.Name))
	if rel, err := filepath.Rel(out, target); err != nil || rel != filepath.Base(target) {
		return "", fmt.Errorf("%w: bad name %q", ErrAttachment, a.Name)
	}
	src := s.path(a.ID)
	if st, err := os.Stat(target); err == nil {
		if sst, err := os.Stat(src); err == nil && sst.Size() == st.Size() {
			return target, nil
		}
	}
	data, err := os.ReadFile(src) //nolint:gosec // G304: a blob of this store
	if err != nil {
		return "", err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return target, nil
}
