package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestSniffAllowlist(t *testing.T) {
	ok := map[string][]byte{
		MIMEPNG:  testPNG(t),
		MIMEJPEG: append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 16)...),
		MIMEGIF:  []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		MIMEWebP: []byte("RIFF\x10\x00\x00\x00WEBPVP8 "),
		MIMEPDF:  []byte("%PDF-1.7\n1 0 obj\n"),
	}
	for want, data := range ok {
		if got, err := sniffBytes(data); err != nil || got != want {
			t.Errorf("sniff %s = %q, %v", want, got, err)
		}
	}
	for _, text := range []string{"plain\n", `{"a":1}`, "a,b\n1,2\n", "# md\n\tкириллица\r\n", "\x1b[31mlog\x1b[0m\n", "<svg></svg>", "package main\n"} {
		if got, err := sniffBytes([]byte(text)); err != nil || got != MIMEText {
			t.Errorf("sniff text %q = %q, %v", text, got, err)
		}
	}
	for name, data := range map[string][]byte{
		"exe":     []byte("MZ\x90\x00\x03\x00\x00\x00"),
		"zip":     []byte("PK\x03\x04\x14\x00\x00\x00"),
		"nul":     []byte("text\x00more"),
		"latin1":  {'c', 'a', 'f', 0xE9},
		"empty":   {},
		"bell":    []byte("ding\x07"),
		"elf":     []byte("\x7fELF\x02\x01\x01"),
		"gzip":    {0x1f, 0x8b, 0x08, 0x00},
		"utf16le": {0xFF, 0xFE, 'a', 0x00},
	} {
		if got, err := sniffBytes(data); !errors.Is(err, ErrAttachmentType) {
			t.Errorf("sniff %s = %q, %v; want ErrAttachmentType", name, got, err)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"pic.png":            "pic.png",
		"../../etc/passwd":   "passwd",
		`..\..\win\evil.png`: "evil.png",
		`C:\x\y.txt`:         "y.txt",
		"a<b>:c|d?e*f\".png": "a_b__c_d_e_f_.png",
		"":                   "file",
		"..":                 "file",
		"...":                "file",
		".hidden":            "hidden",
		"CON":                "_CON",
		"nul.txt":            "_nul.txt",
		"tab\there.md":       "tab_here.md",
		"trail. ":            "trail",
		"отчёт.pdf":          "отчёт.pdf",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeName(strings.Repeat("я", 200) + ".png")
	if len(long) > maxNameLen || !strings.HasSuffix(long, ".png") || !strings.HasPrefix(long, "я") {
		t.Errorf("long name %q (%d bytes)", long, len(long))
	}
}

func TestBodyFallback(t *testing.T) {
	atts := []Attachment{{Name: "a.png"}, {Name: "b.txt"}}
	body := withFallback("hello\n", atts)
	if body != "hello\n[attachment: a.png]\n[attachment: b.txt]" {
		t.Fatalf("body %q", body)
	}
	if got := BodyText(body, atts); got != "hello" {
		t.Fatalf("BodyText %q", got)
	}
	if got := withFallback("", atts[:1]); got != "[attachment: a.png]" || BodyText(got, atts[:1]) != "" {
		t.Fatalf("empty body %q", got)
	}
}

func TestAttachStorePut(t *testing.T) {
	s, err := openAttachStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img := testPNG(t)
	a, err := s.put(bytes.NewReader(img), "../x/pic.png")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != shaHex(img) || a.Name != "pic.png" || a.MIME != MIMEPNG || a.Size != int64(len(img)) || !s.has(a.ID) {
		t.Fatalf("put = %+v", a)
	}
	again, err := s.put(bytes.NewReader(img), "other.png")
	if err != nil || again.ID != a.ID {
		t.Fatalf("dedupe: %+v %v", again, err)
	}
	if d, err := s.describe(a.ID, "named.png"); err != nil || d.MIME != MIMEPNG || d.Name != "named.png" {
		t.Fatalf("describe %+v %v", d, err)
	}
	if _, err := s.put(bytes.NewReader(make([]byte, MaxAttachmentSize+1)), "big.txt"); !errors.Is(err, ErrAttachmentSize) || !errors.Is(err, ErrAttachment) {
		t.Fatalf("oversize: %v", err)
	}
	if _, err := s.put(bytes.NewReader([]byte("MZ\x90\x00")), "tool.png"); !errors.Is(err, ErrAttachmentType) {
		t.Fatalf("exe named png: %v", err)
	}
	if _, err := s.describe(strings.Repeat("0", 64), "x"); !errors.Is(err, ErrAttachment) {
		t.Fatalf("unknown id: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(s.dir, "up-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// transfer sends blob id of src to dst chunk by chunk through mangle (which
// may alter or drop chunks) and reports the last receive result.
func transfer(t *testing.T, src, dst *attachStore, id string, mangle func(i int, c *attChunk) bool) (bool, error) {
	t.Helper()
	var done bool
	var last error
	i := 0
	err := src.chunks(id, func(c attChunk) error {
		c.Data = slices.Clone(c.Data)
		keep := mangle == nil || mangle(i, &c)
		i++
		if !keep {
			return nil
		}
		d, err := dst.receive("peer", c)
		done = done || d
		if err != nil {
			last = err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return done, last
}

func TestAttachTransferChunks(t *testing.T) {
	src, _ := openAttachStore(t.TempDir())
	text := []byte(strings.Repeat("line of text\n", 60000)) // ~780 KB: 3 chunks
	a, err := src.put(bytes.NewReader(text), "log.txt")
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	_ = src.chunks(a.ID, func(c attChunk) error {
		if len(c.Data) > attChunkSize {
			t.Fatalf("chunk of %d bytes", len(c.Data))
		}
		chunks++
		return nil
	})
	if chunks != 3 {
		t.Fatalf("%d chunks", chunks)
	}

	t.Run("ok", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		done, err := transfer(t, src, dst, a.ID, nil)
		if !done || err != nil || !dst.has(a.ID) {
			t.Fatalf("done %v err %v", done, err)
		}
		got, _ := os.ReadFile(dst.path(a.ID))
		if !bytes.Equal(got, text) {
			t.Fatal("content differs")
		}
		// A second push of a blob already here is ignored.
		if done, err := transfer(t, src, dst, a.ID, nil); done || err != nil {
			t.Fatalf("repeat: %v %v", done, err)
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		done, err := transfer(t, src, dst, a.ID, func(i int, c *attChunk) bool {
			if i == 1 {
				c.Data[10] ^= 0xFF
			}
			return true
		})
		if done || err == nil || !strings.Contains(err.Error(), "hash mismatch") || dst.has(a.ID) {
			t.Fatalf("done %v err %v", done, err)
		}
		assertNoPartials(t, dst)
	})
	t.Run("missing chunk", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		done, err := transfer(t, src, dst, a.ID, func(i int, _ *attChunk) bool { return i != 1 })
		if done || err == nil || dst.has(a.ID) {
			t.Fatalf("done %v err %v", done, err)
		}
		assertNoPartials(t, dst)
	})
	t.Run("oversize", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		done, err := transfer(t, src, dst, a.ID, func(_ int, c *attChunk) bool {
			c.Size = MaxAttachmentSize + 1
			return true
		})
		if done || !errors.Is(err, ErrAttachment) || dst.has(a.ID) {
			t.Fatalf("done %v err %v", done, err)
		}
		big := attChunk{ID: a.ID, Size: 2 * attChunkSize, Data: make([]byte, attChunkSize+1)}
		if _, err := dst.receive("peer", big); err == nil {
			t.Fatal("oversized chunk accepted")
		}
		over := attChunk{ID: a.ID, Size: 10, Off: 5, Data: make([]byte, 6)}
		if _, err := dst.receive("peer", over); err == nil {
			t.Fatal("chunk past the size accepted")
		}
		assertNoPartials(t, dst)
	})
	t.Run("type not allowed", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		bin := append([]byte("MZ\x90\x00"), make([]byte, 100)...)
		id := shaHex(bin)
		done, err := dst.receive("peer", attChunk{ID: id, Size: int64(len(bin)), Data: bin})
		if done || !errors.Is(err, ErrAttachmentType) || dst.has(id) {
			t.Fatalf("done %v err %v", done, err)
		}
	})
	t.Run("bad id", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		for _, id := range []string{"../../x", strings.Repeat("A", 64), "abc"} {
			if _, err := dst.receive("peer", attChunk{ID: id, Size: 3, Data: []byte("abc")}); err == nil {
				t.Fatalf("id %q accepted", id)
			}
		}
		entries, _ := os.ReadDir(dst.dir)
		if len(entries) != 0 {
			t.Fatalf("files written: %v", entries)
		}
	})
	t.Run("session end drops", func(t *testing.T) {
		dst, _ := openAttachStore(t.TempDir())
		first := true
		_ = src.chunks(a.ID, func(c attChunk) error {
			if first {
				_, _ = dst.receive("peer", attChunk{ID: c.ID, Size: c.Size, Off: c.Off, Data: slices.Clone(c.Data)})
				first = false
			}
			return nil
		})
		dst.dropPeer("peer")
		assertNoPartials(t, dst)
	})
}

func assertNoPartials(t *testing.T, s *attachStore) {
	t.Helper()
	s.mu.Lock()
	n := len(s.parts)
	s.mu.Unlock()
	left, _ := filepath.Glob(filepath.Join(s.dir, "part-*"))
	if n != 0 || len(left) != 0 {
		t.Fatalf("partials left: %d, files %v", n, left)
	}
}

func TestMaterializePathSafety(t *testing.T) {
	s, _ := openAttachStore(t.TempDir())
	a, err := s.put(strings.NewReader("hello\n"), "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	out := filepath.Join(work, ".agentlink", "attachments")
	for _, name := range []string{"../../evil.txt", `..\..\evil.txt`, "..", "/abs/x.txt", "C:\\x.txt", "a/../../b.txt"} {
		a.Name = name
		p, err := s.materialize(a, work)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if filepath.Dir(p) != out || !strings.HasPrefix(filepath.Base(p), a.ID[:8]+"-") {
			t.Fatalf("%q materialized at %s", name, p)
		}
		if got, _ := os.ReadFile(filepath.Clean(p)); string(got) != "hello\n" {
			t.Fatalf("%q content %q", name, got)
		}
	}
	ign, err := os.ReadFile(filepath.Join(work, ".agentlink", ".gitignore")) //nolint:gosec // G304: the test's own temp folder
	if err != nil || string(ign) != "*\n" {
		t.Fatalf(".gitignore %q %v", ign, err)
	}
	if _, err := os.Stat(filepath.Join(work, "evil.txt")); err == nil {
		t.Fatal("escaped the folder")
	}
}

// Attaching a local file is limited to the node's folders and the temp folder.
func TestAllowedFile(t *testing.T) {
	base := t.TempDir()
	tmp, work, other := filepath.Join(base, "tmp"), filepath.Join(base, "work"), filepath.Join(base, "other")
	for _, d := range []string{tmp, work, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, tmp)
	}
	n := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	n.SetFolders(work, nil)
	write := func(dir string) string {
		p := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, p := range []string{write(tmp), write(work)} {
		if _, err := n.allowedFile(p, ""); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
	if _, err := n.allowedFile("f.txt", work); err != nil {
		t.Errorf("relative to folder refused: %v", err)
	}
	for _, p := range []string{write(other), filepath.Join(work, "..", "other", "f.txt"), work} {
		if _, err := n.allowedFile(p, ""); !errors.Is(err, ErrAttachment) {
			t.Errorf("%s allowed: %v", p, err)
		}
	}
	if _, err := n.resolveAttachments(SendRequest{Files: []string{write(other)}}); !errors.Is(err, ErrAttachmentPath) {
		t.Errorf("resolve outside: %v", err)
	}
	many := make([]string, MaxAttachments+1)
	if _, err := n.resolveAttachments(SendRequest{Files: many}); !errors.Is(err, ErrAttachmentSize) {
		t.Errorf("too many: %v", err)
	}
}

// A peer without CapAttachments is sent no att frames (the message alone,
// with its fallback lines).
func TestPushSkipsOldPeers(t *testing.T) {
	n := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	a, err := n.atts.put(strings.NewReader("x\n"), "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	pc := &peerConn{peer: "old", caps: []string{CapCaps, CapChat}} // no wire: a write would panic
	if err := n.pushAttachments(pc, &Message{Attachments: []Attachment{a}}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
}

// Two nodes: b attaches an image from its temp folder; a receives the blob
// before the message, verified, and lists it for its agents by a path inside
// its project folder, in the unread page and in the wake prompt.
func TestAttachmentEndToEnd(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	workA := t.TempDir()
	a.SetFolders(workA, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	if !a.PeerHas("b", CapAttachments) {
		t.Fatal("no attachments capability")
	}
	img := testPNG(t)
	file := filepath.Join(t.TempDir(), "screen shot.png")
	if err := os.WriteFile(file, img, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := b.SendRequest(SendRequest{To: "a", Body: "look", Files: []string{file}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "look\n[attachment: screen shot.png]" || len(m.Attachments) != 1 || m.Attachments[0].ID != shaHex(img) {
		t.Fatalf("sent %q %+v", m.Body, m.Attachments)
	}
	var got UnreadMessage
	eventually(t, "a has the message", func() bool {
		p, _ := a.Unread("", "", 10)
		for _, u := range p.Messages {
			if u.ID == m.ID {
				got = u
				return true
			}
		}
		return false
	})
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments %+v", got.Attachments)
	}
	att := got.Attachments[0]
	wantDir := filepath.Join(workA, ".agentlink", "attachments")
	if att.Failed || att.MIME != MIMEPNG || att.Size != int64(len(img)) || filepath.Dir(att.Path) != wantDir ||
		filepath.Base(att.Path) != att.ID[:8]+"-screen shot.png" {
		t.Fatalf("attachment %+v", att)
	}
	onDisk, err := os.ReadFile(filepath.Clean(att.Path))
	if err != nil || !bytes.Equal(onDisk, img) {
		t.Fatalf("materialized file: %v", err)
	}
	text, images := WakePrompt([]UnreadMessage{got}, 0, workA, "t"), wakeImages([]UnreadMessage{got})
	if !strings.Contains(text, att.Path) || !slices.Equal(images, []string{att.Path}) {
		t.Fatalf("wake prompt %q images %v", text, images)
	}
	// The author sees its own attachment; the reader's history shows it too.
	for _, n := range []*testNode{a, b} {
		msgs, err := n.ChatMessages(m.ChatID, 0, 0, 10)
		if err != nil || len(msgs) == 0 {
			t.Fatalf("%s history: %v", n.cfg.Node, err)
		}
		last := msgs[len(msgs)-1]
		if len(last.Attachments) != 1 || last.Attachments[0].Failed || last.Attachments[0].Path != "" {
			t.Fatalf("%s history attachments %+v", n.cfg.Node, last.Attachments)
		}
	}
}

// A message whose blob never arrived shows its attachment as failed.
func TestAttachmentMissingBlobFails(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	c, sc := manualDial(t, a, "b", nil, `,"proto":6,"caps":["caps","hb"]`)
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	id := strings.Repeat("ab", 32)
	m := Message{ID: newID(), From: "b", To: "a", Body: "see [attachment: x.png]", CreatedAt: time.Now().UTC(),
		Attachments: []Attachment{{ID: id, Name: "../x.png", MIME: "application/x-evil", Size: 5, Path: "C:\\evil", Failed: false}}}
	if err := writeFrame(c, frame{Type: "msg", Msg: &m}); err != nil {
		t.Fatal(err)
	}
	awaitAck(t, c, sc, m.ID)
	p, err := a.Unread("", "", 10)
	if err != nil || len(p.Messages) != 1 {
		t.Fatalf("unread %+v %v", p, err)
	}
	got := p.Messages[0].Attachments
	if len(got) != 1 || !got[0].Failed || got[0].Name != "x.png" || got[0].MIME != "" || got[0].Path != "" {
		t.Fatalf("attachment %+v", got)
	}
}
