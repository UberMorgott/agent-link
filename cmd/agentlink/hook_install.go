package main

// agentlink hook install: adds the agentlink hook to the settings of Claude
// Code (~/.claude/settings.json, .claude/settings.json) or Codex
// (~/.codex/hooks.json, .codex/hooks.json); see package agenthook.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/UberMorgott/agent-link/internal/agenthook"
)

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
			if changed, err = agenthook.Install(path, client, exe); err == nil {
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
