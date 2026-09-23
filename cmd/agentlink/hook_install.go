package main

// agentlink hook install: adds the agentlink hook to the settings of Claude
// Code (~/.claude/settings.json, .claude/settings.json) or Codex
// (~/.codex/hooks.json, .codex/hooks.json). The file keeps its other content
// and key order; the previous version is saved next to it as *.agentlink.bak.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// hookTimeout is the timeout (seconds) of an installed hook entry.
const hookTimeout = 10

func runHookInstall(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != hookClaude && args[0] != hookCodex) {
		_, _ = fmt.Fprintln(stderr, "usage: agentlink hook install <claude|codex> [--scope user|project]")
		return 1
	}
	client := args[0]
	flags := flag.NewFlagSet("hook install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scope := flags.String("scope", "user", "user: the user's settings; project: the settings of the current directory")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	path, err := hookSettingsPath(client, *scope)
	if err == nil {
		var exe string
		if exe, err = os.Executable(); err == nil {
			var changed bool
			if changed, err = installHook(path, client, exe); err == nil {
				msg := "agentlink hook for %s is already in %s\n"
				if changed {
					msg = "agentlink hook for %s added to %s\n"
				}
				_, _ = fmt.Fprintf(stdout, msg, client, path)
				if client == hookCodex && changed {
					_, _ = fmt.Fprintln(stdout, "Codex runs a new hook only after you review and trust it: open /hooks in Codex.")
				}
				return 0
			}
		}
	}
	_, _ = fmt.Fprintln(stderr, "agentlink:", err)
	return 1
}

// hookSettingsPath is the file the hooks of client live in for scope.
func hookSettingsPath(client, scope string) (string, error) {
	var base string
	switch scope {
	case "user":
		if client == hookCodex && os.Getenv("CODEX_HOME") != "" {
			return filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = home
	case "project":
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = wd
	default:
		return "", fmt.Errorf("invalid --scope %q: user or project", scope)
	}
	if client == hookCodex {
		return filepath.Join(base, ".codex", "hooks.json"), nil
	}
	return filepath.Join(base, ".claude", "settings.json"), nil
}

// hookEntry is the hook handler agentlink installs for client. Claude Code
// spawns an exec-form entry (command + args) without a shell, so the path
// needs no quoting; Codex hands command to a shell.
func hookEntry(client, exe string) hookHandler {
	exe = filepath.ToSlash(exe)
	if client == hookClaude {
		return hookHandler{Type: "command", Command: exe, Args: []string{"hook", client}, Timeout: hookTimeout}
	}
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"`
	}
	return hookHandler{Type: "command", Command: exe + " hook " + client, Timeout: hookTimeout}
}

// hookHandler is one installed hook handler, in the key order people write.
type hookHandler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout"`
}

// isAgentlinkHook reports whether a matcher group runs `agentlink hook client`.
func isAgentlinkHook(group json.RawMessage, client string) bool {
	var g struct {
		Hooks []struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"hooks"`
	}
	if json.Unmarshal(group, &g) != nil {
		return false
	}
	for _, h := range g.Hooks {
		line := strings.ToLower(h.Command + " " + strings.Join(h.Args, " "))
		if strings.Contains(line, "agentlink") && strings.Contains(line, "hook "+client) {
			return true
		}
	}
	return false
}

// installHook adds (or updates) the agentlink matcher group of every hook
// event in the settings file at path. It reports whether the file changed.
func installHook(path, client, exe string) (bool, error) {
	old, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	src := bytes.TrimSpace(old)
	if len(src) == 0 {
		src = []byte("{}")
	}
	top, err := decodeObject(src)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	hooksRaw, _ := top.get("hooks")
	if hooksRaw == nil {
		hooksRaw = json.RawMessage("{}")
	}
	hooks, err := decodeObject(hooksRaw)
	if err != nil {
		return false, fmt.Errorf("%s: hooks: %w", path, err)
	}
	entry := hookEntry(client, exe)
	for _, ev := range hookEvents {
		group := struct {
			Matcher string        `json:"matcher,omitempty"`
			Hooks   []hookHandler `json:"hooks"`
		}{Hooks: []hookHandler{entry}}
		if ev == evPostTool {
			group.Matcher = "*"
		}
		want := json.RawMessage(bytes.TrimSpace(mustJSON(group)))
		var groups []json.RawMessage
		if raw, ok := hooks.get(ev); ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return false, fmt.Errorf("%s: hooks.%s: %w", path, ev, err)
			}
		}
		replaced := false
		for i, g := range groups {
			if isAgentlinkHook(g, client) {
				groups[i], replaced = want, true
				break
			}
		}
		if !replaced {
			groups = append(groups, want)
		}
		hooks.set(ev, mustJSON(groups))
	}
	top.set("hooks", hooks.encode())
	var out bytes.Buffer
	if err := json.Indent(&out, top.encode(), "", "  "); err != nil {
		return false, err
	}
	out.WriteByte('\n')
	if len(old) > 0 && jsonEqual(old, out.Bytes()) {
		return false, nil
	}
	if len(old) > 0 {
		if err := os.WriteFile(path+".agentlink.bak", old, 0o600); err != nil {
			return false, err
		}
	}
	return true, writeFileAtomic(path, out.Bytes())
}

// jsonEqual reports whether a and b hold the same JSON, whitespace aside.
func jsonEqual(a, b []byte) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}

// object is a JSON object that keeps its key order and raw values.
type object []struct {
	key string
	val json.RawMessage
}

func decodeObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		o.set(key, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	for _, kv := range o {
		if kv.key == key {
			return kv.val, true
		}
	}
	return nil, false
}

func (o *object) set(key string, val []byte) {
	val = bytes.TrimSpace(val)
	for i := range *o {
		if (*o)[i].key == key {
			(*o)[i].val = val
			return
		}
	}
	*o = append(*o, struct {
		key string
		val json.RawMessage
	}{key, val})
}

func (o object) encode() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(bytes.TrimSpace(mustJSON(kv.key)))
		b.WriteByte(':')
		b.Write(kv.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}
