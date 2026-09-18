package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Plain-line helpers for tests that play a legacy peer or read a handshake
// by hand.

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)
	return sc
}

// readFrame returns the next known handshake frame, which must be of type want.
func readFrame(sc *bufio.Scanner, want string) (frame, error) {
	for sc.Scan() {
		f, ok := decodeFrame(sc.Bytes())
		if !ok || !handshakeFrames[f.Type] {
			continue
		}
		if f.Type != want {
			return frame{}, fmt.Errorf("expected %q frame, got %q", want, f.Type)
		}
		return f, nil
	}
	if err := sc.Err(); err != nil {
		return frame{}, err
	}
	return frame{}, io.EOF
}

func writeFrame(w io.Writer, f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
