package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unicode/utf16"
	"unicode/utf8"
)

// The CLI's JSON reaches agents through a pipe, and a shell may decode a
// program's output with the console's code page: PowerShell with cp866 turns
// the UTF-8 of "—" into "тАФ" in `$h = agentlink chat history …`, and a reply
// built from it carries that. JSON escapes (—) are plain ASCII, the same
// in every code page, and every JSON parser (ConvertFrom-Json too) restores
// the text; a console gets the text itself (Go writes it as UTF-16 there).

// newJSONEncoder is a JSON encoder for w that escapes every non-ASCII
// character unless w is a console.
func newJSONEncoder(w io.Writer) *json.Encoder {
	if f, ok := w.(*os.File); !ok || !isConsole(f) {
		w = asciiJSON{w}
	}
	return json.NewEncoder(w)
}

// asciiJSON writes JSON text with every non-ASCII character as a \u escape
// (a UTF-16 surrogate pair beyond the BMP). Outside strings JSON is ASCII, so
// only strings change, and to the same value. Each Write is whole JSON text
// (json.Encoder writes a value at once), so no character is split.
type asciiJSON struct{ w io.Writer }

func (a asciiJSON) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+len(p)/2)
	for i := 0; i < len(p); {
		if p[i] < utf8.RuneSelf {
			out = append(out, p[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(p[i:])
		i += size
		if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
			out = fmt.Appendf(out, `\u%04x\u%04x`, r1, r2)
			continue
		}
		out = fmt.Appendf(out, `\u%04x`, r)
	}
	if _, err := a.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
