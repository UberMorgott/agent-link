package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Message text from a file or stdin (--body-file): a shell never sees it, so
// quotes, "", backticks, $, %, ^ and newlines arrive as written. A command
// line would let the shell (PowerShell, cmd) rewrite or split them.

// cliStdin is what --body-file - reads (a test replaces it).
var cliStdin io.Reader = os.Stdin

// maxBodyFile bounds the text read from a file or stdin.
const maxBodyFile = 16 << 20

// messageText is the message text of a command: body, or the contents of file
// (flag names the file's flag in errors; "-" reads stdin). Both at once is an
// error.
func messageText(body, file, flag string) (string, error) {
	if file == "" {
		return body, nil
	}
	if body != "" {
		return "", fmt.Errorf("--body and %s cannot be used together", flag)
	}
	r := cliStdin
	if file != "-" {
		f, err := os.Open(file) //nolint:gosec // G304: the caller's own message file
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBodyFile+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBodyFile {
		return "", fmt.Errorf("%s: text larger than %d MB", flag, maxBodyFile>>20)
	}
	text, err := decodeText(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	// A file or a pipe ends with a line break the text does not mean.
	return strings.TrimRight(text, "\r\n"), nil
}

// decodeText is data as text: UTF-8 (a BOM dropped), or UTF-16 with its BOM
// (Windows PowerShell's Out-File and > write that). Anything else is refused:
// a guessed code page would garble the message silently.
func decodeText(data []byte) (string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0, 0}), bytes.HasPrefix(data, []byte{0, 0, 0xFE, 0xFF}):
		return "", errors.New("UTF-32 text (save the file as UTF-8)")
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		data = data[3:]
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		le := data[0] == 0xFF
		data = data[2:]
		if len(data)%2 != 0 {
			return "", errors.New("odd-length UTF-16 text")
		}
		u := make([]uint16, len(data)/2)
		for i := range u {
			if le {
				u[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
			} else {
				u[i] = uint16(data[2*i])<<8 | uint16(data[2*i+1])
			}
		}
		for i := 0; i < len(u); i++ {
			switch {
			case utf16.IsSurrogate(rune(u[i])) && u[i] < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] <= 0xDFFF:
				i++ // a pair
			case utf16.IsSurrogate(rune(u[i])):
				return "", errors.New("broken UTF-16 text (an unpaired surrogate)")
			}
		}
		return string(utf16.Decode(u)), nil
	}
	if !utf8.Valid(data) {
		return "", errors.New("not UTF-8 text (save the file as UTF-8)")
	}
	return string(data), nil
}
