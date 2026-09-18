package node

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/chacha20poly1305"
)

// A PAKE session (both sides announced CapPAKE and ran the CPace handshake)
// switches to sealed records right after the dialer's auth frame, starting
// with the acceptor's ok. Every later frame, in both directions, is one record:
//
//	length  uint32, big endian: len(sealed), at most maxRecord
//	sealed  ChaCha20-Poly1305(key, nonce, frame JSON, ad = length)
//
// Each direction has its own key (channelKeys: HKDF of the CPace ISK, salted
// with the hash of both hello lines) and its own record counter, starting at
// 0; the 12-byte nonce is 4 zero bytes and the counter, big endian. The
// counter never travels: a dropped, replayed, reordered, reflected or altered
// record fails to open, and the connection is closed. After maxRecords
// records in one direction the connection is closed rather than a nonce reused.
//
// A legacy session (pre-v0.6 peer, private addresses only) stays on plain
// newline-delimited JSON.
const (
	recordHeader = 4
	maxRecord    = maxFrame + chacha20poly1305.Overhead
	// maxRecords bounds the records sent or received under one key, far
	// below the 2^64 nonces and within the AEAD's usage limits.
	maxRecords = 1 << 48
	// maxHandshakeLine bounds a plain line before a session is authenticated.
	maxHandshakeLine = 64 << 10
)

var (
	// errRecord: a record failed to open (altered, replayed, reordered or
	// sealed under another key) or has an impossible length.
	errRecord = errors.New("record authentication failed")
	// errExhausted: a direction used up its record counter.
	errExhausted = errors.New("record counter exhausted")
)

// sealer is one direction of a sealed session.
type sealer struct {
	aead cipher.AEAD
	seq  uint64
}

func newSealer(key []byte) (*sealer, error) {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: a}, nil
}

func (s *sealer) nonce() ([]byte, error) {
	if s.seq >= maxRecords {
		return nil, errExhausted
	}
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(n[4:], s.seq)
	s.seq++
	return n, nil
}

// seal returns the record carrying p.
func (s *sealer) seal(p []byte) ([]byte, error) {
	if len(p) > maxFrame {
		return nil, fmt.Errorf("frame of %d bytes too large", len(p))
	}
	nonce, err := s.nonce()
	if err != nil {
		return nil, err
	}
	rec := make([]byte, recordHeader, recordHeader+len(p)+chacha20poly1305.Overhead)
	binary.BigEndian.PutUint32(rec, uint32(len(p)+chacha20poly1305.Overhead)) //nolint:gosec // G115: len(p) <= maxFrame (1 MiB), checked above
	return s.aead.Seal(rec, nonce, p, rec[:recordHeader]), nil
}

// open reads and opens the next record from r.
func (s *sealer) open(r io.Reader) ([]byte, error) {
	var hdr [recordHeader]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size < chacha20poly1305.Overhead || size > maxRecord {
		return nil, errRecord
	}
	sealed := make([]byte, size)
	if _, err := io.ReadFull(r, sealed); err != nil {
		return nil, err
	}
	nonce, err := s.nonce()
	if err != nil {
		return nil, err
	}
	p, err := s.aead.Open(sealed[:0], nonce, sealed, hdr[:])
	if err != nil {
		return nil, errRecord
	}
	return p, nil
}

// wire carries the frames of one connection: plain lines until secure,
// sealed records after. Reads and writes may run concurrently; writes are
// serialized by the caller (peerConn.wmu), reads by having one reader.
type wire struct {
	c        net.Conn
	r        *bufio.Reader
	maxLine  int
	out, in  *sealer // nil: plain
	lastLine []byte  // the last plain line read, for the handshake transcript
}

func newWire(c net.Conn) *wire {
	return &wire{c: c, r: bufio.NewReaderSize(c, 64<<10), maxLine: maxHandshakeLine}
}

// secure switches both directions to sealed records.
func (w *wire) secure(k channelKeys, dialer bool) error {
	send, recv := k.dialerToAcceptor, k.acceptorToDialer
	if !dialer {
		send, recv = recv, send
	}
	out, err := newSealer(send)
	if err != nil {
		return err
	}
	in, err := newSealer(recv)
	if err != nil {
		return err
	}
	w.out, w.in = out, in
	return nil
}

func (w *wire) sealed() bool { return w.out != nil }

// next returns the next frame's bytes. Any error ends the session.
func (w *wire) next() ([]byte, error) {
	if w.in != nil {
		return w.in.open(w.r)
	}
	return w.readLine()
}

// readLine reads one newline-terminated line of at most maxLine bytes; the
// newline and a trailing CR are dropped, and a last line without a newline
// is returned at EOF.
func (w *wire) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := w.r.ReadSlice('\n')
		if len(line)+len(chunk) > w.maxLine+1 {
			return nil, bufio.ErrTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return bytes.TrimSuffix(line[:len(line)-1], []byte("\r")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return bytes.TrimSuffix(line, []byte("\r")), nil
		default:
			return nil, err
		}
	}
}

// write sends one frame.
func (w *wire) write(f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return w.writeData(data)
}

func (w *wire) writeData(data []byte) error {
	if w.out != nil {
		rec, err := w.out.seal(data)
		if err != nil {
			return err
		}
		_, err = w.c.Write(rec)
		return err
	}
	_, err := w.c.Write(append(data, '\n'))
	return err
}

// readFrame returns the next handshake frame, which must be of type want.
// Frames of a type the handshake does not know (a newer peer's extras) and
// frames that do not parse are skipped; the handshake deadline bounds the
// wait. The frame's raw line is kept in lastLine.
func (w *wire) readFrame(want string) (frame, error) {
	for {
		data, err := w.next()
		if err != nil {
			return frame{}, err
		}
		f, ok := decodeFrame(data)
		if !ok || !handshakeFrames[f.Type] {
			continue
		}
		if f.Type != want {
			return frame{}, fmt.Errorf("expected %q frame, got %q", want, f.Type)
		}
		w.lastLine = data
		return f, nil
	}
}
