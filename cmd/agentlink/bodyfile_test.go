package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/UberMorgott/agent-link/internal/node"
)

// bodyHelperEnv makes TestBodyCLIHelper run the CLI with the arguments after
// "--": the test binary stands in for agentlink.exe behind a real shell.
const bodyHelperEnv = "AGENTLINK_TEST_BODY_HELPER"

func TestBodyCLIHelper(t *testing.T) {
	if os.Getenv(bodyHelperEnv) != "1" {
		t.Skip("run by TestSendBodyThroughShells")
	}
	i := slices.Index(os.Args, "--")
	if i < 0 {
		os.Exit(3)
	}
	os.Exit(run(os.Args[i+1:], os.Stdout, os.Stderr))
}

// hardBody is a message a shell would mangle on a command line.
func hardBody() string {
	line := `Glob("", "*.txt") 'single' "double" ""empty"" ` + "`backtick` $env:PATH $(whoami) @(1) %PATH% ^caret & | < > ; \\ \\\" \t tab\r\n" +
		"второй «ряд» ünïcödé 🙂 — \\\\server\\share\n"
	var b strings.Builder
	for b.Len() < 20<<10 {
		b.WriteString(line)
	}
	return b.String() + "end"
}

func TestMessageText(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	utf16le := func(s string) []byte {
		b := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(s)) {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
		return b
	}
	utf16be := func(s string) []byte {
		b := []byte{0xFE, 0xFF}
		for _, u := range utf16.Encode([]rune(s)) {
			b = binary.BigEndian.AppendUint16(b, u)
		}
		return b
	}
	body := hardBody()
	for name, data := range map[string][]byte{
		"plain.txt": []byte(body),
		"nl.txt":    []byte(body + "\r\n"),
		"bom8.txt":  append([]byte{0xEF, 0xBB, 0xBF}, body...),
		"le.txt":    utf16le(body + "\r\n"), // Windows PowerShell's Out-File
		"be.txt":    utf16be(body),
	} {
		got, err := messageText("", write(name, data), "--body-file")
		if err != nil || got != body {
			t.Fatalf("%s: %v, %d runes, want %d", name, err, len([]rune(got)), len([]rune(body)))
		}
	}
	old := cliStdin
	t.Cleanup(func() { cliStdin = old })
	cliStdin = strings.NewReader(body + "\n")
	if got, err := messageText("", "-", "--body-file"); err != nil || got != body {
		t.Fatalf("stdin: %v", err)
	}
	if got, err := messageText("x", "", "--body-file"); err != nil || got != "x" {
		t.Fatalf("no file: %q %v", got, err)
	}
	for _, c := range []struct{ body, file, want string }{
		{"x", write("a.txt", []byte("y")), "cannot be used together"},
		{"", write("bad.txt", []byte{0xC0, 0x41}), "not UTF-8"},
		{"", write("odd.txt", []byte{0xFF, 0xFE, 0x41}), "odd-length"},
		{"", write("utf32.txt", []byte{0xFF, 0xFE, 0, 0, 0x41, 0, 0, 0}), "UTF-32"},
		{"", write("lone.txt", []byte{0xFF, 0xFE, 0x00, 0xD8}), "unpaired"},
		{"", write("lowfirst.txt", []byte{0xFF, 0xFE, 0x00, 0xDC, 0x00, 0xD8}), "unpaired"},
		{"", filepath.Join(dir, "missing.txt"), "missing.txt"},
	} {
		if _, err := messageText(c.body, c.file, "--body-file"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q: %v, want %q", c.file, err, c.want)
		}
	}
}

// A value the shell split leaves extra arguments: the command fails instead
// of sending the text cut at the split.
func TestSendRejectsSplitBody(t *testing.T) {
	f := newFakeAPI(t)
	var out, errw bytes.Buffer
	if code := run([]string{"send", "--chat", "c1", "--body", "Glob(", ",", "*.txt)", "--config", f.cfg}, &out, &errw); code == 0 ||
		!strings.Contains(errw.String(), `unexpected argument ","`) || !strings.Contains(errw.String(), "--body-file") {
		t.Fatalf("code %d, stderr %s", code, errw.String())
	}
	if len(f.reqs) != 0 {
		t.Fatalf("sent anyway: %v", f.reqs)
	}
	// --body-file on send and discuss.
	file := filepath.Join(t.TempDir(), "q.txt")
	if err := os.WriteFile(file, []byte(hardBody()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.run("send", "--chat", "c1", "--body-file", file)
	if f.send.Body != hardBody() {
		t.Fatalf("send body: %d runes", len([]rune(f.send.Body)))
	}
	f.run("discuss", "--with", "codex", "--body-file", file)
	if f.discuss["body"] != hardBody() {
		t.Fatalf("discuss body: %d runes", len([]rune(f.discuss["body"])))
	}
	if code := run([]string{"discuss", "--with", "codex", "--body-file", file, "--prompt-file", file, "--config", f.cfg}, &out, &errw); code == 0 {
		t.Fatal("--body-file with --prompt-file accepted")
	}
}

// The message text goes through real shells (pwsh, Windows PowerShell, cmd)
// into the CLI and reaches the API byte for byte; a --body the shell mangles
// either arrives intact or fails, never cut short.
func TestSendBodyThroughShells(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows shells")
	}
	if testing.Short() {
		t.Skip("starts shells")
	}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req node.SendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		bodies = append(bodies, req.Body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(node.Message{ID: "m1"})
	}))
	t.Cleanup(srv.Close)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "body.txt")
	body := hardBody()
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := `-test.run=TestBodyCLIHelper -- send --chat c1`
	batch := func(name, line string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("@"+line+"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	type shellRun struct {
		name  string
		args  []string
		exact bool // must arrive intact; else intact or a failure
	}
	var runs []shellRun
	for _, ps := range []string{"pwsh", "powershell"} {
		if _, err := exec.LookPath(ps); err != nil {
			t.Logf("%s not found: skipped", ps)
			continue
		}
		cmd := func(s string) []string { return []string{"-NoProfile", "-NonInteractive", "-Command", s} }
		cli := `'-test.run=TestBodyCLIHelper' -- send --chat c1` // PowerShell splits a bare -a.b=c
		runs = append(runs,
			shellRun{ps + " file", cmd(`& $env:T_EXE ` + cli + ` --body-file $env:T_FILE; exit $LASTEXITCODE`), true},
			shellRun{ps + " quoted body", cmd(`& $env:T_EXE ` + cli + ` --body 'Glob("", "*.txt") done'; exit $LASTEXITCODE`), false},
			shellRun{ps + " legacy quoted body", cmd(`$PSNativeCommandArgumentPassing = 'Legacy'; & $env:T_EXE ` + cli + ` --body 'Glob("", "*.txt") done'; exit $LASTEXITCODE`), false},
		)
		if ps == "pwsh" { // UTF-8 pipes (Windows PowerShell pipes ASCII)
			runs = append(runs, shellRun{ps + " stdin", cmd(`Get-Content -Raw -LiteralPath $env:T_FILE | & $env:T_EXE ` + cli + ` --body-file -; exit $LASTEXITCODE`), true})
		}
	}
	runs = append(runs,
		shellRun{"cmd file", []string{"/d", "/c", batch("file.cmd", `"%T_EXE%" `+cli+` --body-file "%T_FILE%"`)}, true},
		shellRun{"cmd stdin", []string{"/d", "/c", batch("stdin.cmd", `"%T_EXE%" `+cli+` --body-file - < "%T_FILE%"`)}, true},
		shellRun{"cmd quoted body", []string{"/d", "/c", batch("quoted.cmd", `"%T_EXE%" `+cli+` --body "Glob("", "*.txt") done"`)}, false},
	)
	for _, r := range runs {
		mu.Lock()
		bodies = nil
		mu.Unlock()
		shell := strings.Fields(r.name)[0]
		c := exec.CommandContext(t.Context(), shell, r.args...) //nolint:gosec // G204: a test's own shell command
		c.Env = append(os.Environ(), bodyHelperEnv+"=1", "T_EXE="+self, "T_FILE="+file,
			envAPI+"="+strings.TrimPrefix(srv.URL, "http://"))
		out, err := c.CombinedOutput()
		mu.Lock()
		got := slices.Clone(bodies)
		mu.Unlock()
		want := body
		if !r.exact {
			want = `Glob("", "*.txt") done`
		}
		switch {
		case err == nil && len(got) == 1 && got[0] == want:
			t.Logf("%s: intact", r.name)
		case r.exact:
			t.Fatalf("%s: err %v, sent %d, output %s", r.name, err, len(got), out)
		case err != nil && len(got) == 0:
			t.Logf("%s: refused: %s", r.name, strings.TrimSpace(string(out)))
		default:
			t.Fatalf("%s: err %v, sent %q: the text changed silently", r.name, err, got)
		}
	}
}
