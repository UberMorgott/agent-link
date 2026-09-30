package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// JSON for a pipe is ASCII: a shell decoding it with any code page (cp866)
// reads it intact, and parsing gives the text back.
func TestJSONOutputIsASCII(t *testing.T) {
	body := "ZEBRA-7731 — ответ «ok» 😀 <b>&"
	var buf bytes.Buffer
	if err := encodeLines(&buf, []node.Message{{ID: "m1", Body: body}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, c := range out {
		if c > 0x7f {
			t.Fatalf("non-ASCII output %q", out)
		}
	}
	bs := "\\" + "u"
	if !strings.Contains(out, bs+"2014") || !strings.Contains(out, bs+"d83d"+bs+"de00") {
		t.Fatalf("escapes missing: %q", out)
	}
	var m node.Message
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil || m.Body != body {
		t.Fatalf("round trip %q %v", m.Body, err)
	}
}

// Text on stdin that is not UTF-8 is refused with the way to send it right,
// naming a console code page that is not UTF-8.
func TestStdinNotUTF8Hint(t *testing.T) {
	old := cliStdin
	t.Cleanup(func() { cliStdin = old })
	cliStdin = bytes.NewReader([]byte{0xE2, 0x80, 0x94, 0xFF})
	_, err := messageText("", "-", "--body-file")
	if err == nil || !strings.Contains(err.Error(), "not UTF-8") || !strings.Contains(err.Error(), "$OutputEncoding") {
		t.Fatalf("err %v", err)
	}
	if h := stdinHint(866); !strings.Contains(h, "code page 866") {
		t.Fatalf("hint %q", h)
	}
	if h := stdinHint(65001); strings.Contains(h, "code page") {
		t.Fatalf("hint %q", h)
	}
}
