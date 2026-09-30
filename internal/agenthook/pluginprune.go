package agenthook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PluginGrace is how long an old agent-link plugin copy stays in a client's
// cache after it stopped being the installed one: a session started before
// the update still runs its hooks from there (${CLAUDE_PLUGIN_ROOT}), and
// removing them would break every hook of that live session.
const PluginGrace = 7 * 24 * time.Hour

// trashPrefix names a copy being removed: it is first renamed (which Windows
// refuses while any file in it is open, so a copy in use is kept) and then
// deleted; a deletion cut short is finished at the next prune.
const trashPrefix = ".agentlink-removing-"

// orphanedMarker is the file Claude Code writes into a plugin copy it no
// longer uses.
const orphanedMarker = ".orphaned_at"

// PrunePluginCaches removes old copies of the agent-link plugin from the
// Claude Code and Codex plugin caches and returns the folders it removed.
// Only agent-link's own copies go (a folder under
// plugins/cache/<marketplace>/agent-link whose manifest names agent-link),
// only when the client no longer uses them (Claude Code: not an installPath
// in installed_plugins.json; Codex: not the version this executable expects,
// which must itself be there), only after PluginGrace, and only when nothing
// in them is in use.
func PrunePluginCaches(now time.Time) (removed []string, err error) {
	if dir := configDir("CLAUDE_CONFIG_DIR", ".claude"); dir != "" {
		r, e := pruneClaudeCache(dir, now)
		removed, err = append(removed, r...), errors.Join(err, e)
	}
	if home := configDir("CODEX_HOME", ".codex"); home != "" {
		r, e := pruneCodexCache(home, CheckPlugin(Codex).Want, now)
		removed, err = append(removed, r...), errors.Join(err, e)
	}
	return removed, err
}

// pluginCopies lists the version folders of agent-link under cache, by
// marketplace, and removes what an earlier prune left half-deleted.
func pluginCopies(cache string) (copies []string, err error) {
	markets, _ := os.ReadDir(cache)
	for _, m := range markets {
		if !m.IsDir() {
			continue
		}
		root := filepath.Join(cache, m.Name(), PluginName)
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			path := filepath.Join(root, e.Name())
			switch {
			case !e.IsDir():
			case strings.HasPrefix(e.Name(), trashPrefix):
				err = errors.Join(err, os.RemoveAll(path))
			case isAgentLinkCopy(path):
				copies = append(copies, path)
			}
		}
	}
	return copies, err
}

// isAgentLinkCopy: the folder is a copy of this plugin (either manifest names
// it), not anything else a person may keep there.
func isAgentLinkCopy(dir string) bool {
	for _, m := range []string{".claude-plugin", ".codex-plugin"} {
		var v struct {
			Name string `json:"name"`
		}
		if readJSON(filepath.Join(dir, m, "plugin.json"), &v) && v.Name == PluginName {
			return true
		}
	}
	return false
}

// unusedSince is when the copy stopped being used: Claude Code's orphan
// marker, else the folder's own time.
func unusedSince(dir string) time.Time {
	if st, err := os.Stat(filepath.Join(dir, orphanedMarker)); err == nil {
		return st.ModTime()
	}
	if st, err := os.Stat(dir); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// removeCopy renames the copy aside (refused while a file in it is open) and
// deletes it.
func removeCopy(dir string) error {
	trash := filepath.Join(filepath.Dir(dir), trashPrefix+filepath.Base(dir))
	if err := os.Rename(dir, trash); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

func pruneClaudeCache(dir string, now time.Time) (removed []string, err error) {
	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if !readJSON(filepath.Join(dir, "plugins", "installed_plugins.json"), &installed) {
		return nil, nil // no record: nothing is known to be unused
	}
	used := map[string]bool{}
	for _, list := range installed.Plugins {
		for _, in := range list {
			if in.InstallPath != "" {
				used[strings.ToLower(filepath.Clean(in.InstallPath))] = true
			}
		}
	}
	copies, err := pluginCopies(filepath.Join(dir, "plugins", "cache"))
	for _, c := range copies {
		if used[strings.ToLower(filepath.Clean(c))] || now.Sub(unusedSince(c)) < PluginGrace {
			continue
		}
		if e := removeCopy(c); e != nil {
			err = errors.Join(err, e)
			continue
		}
		removed = append(removed, c)
	}
	return removed, err
}

func pruneCodexCache(home, want string, now time.Time) (removed []string, err error) {
	if want == "" {
		return nil, nil
	}
	copies, err := pluginCopies(filepath.Join(home, "plugins", "cache"))
	current := map[string]bool{} // marketplace folders holding the expected copy
	for _, c := range copies {
		if manifestVersion(c) == want {
			current[filepath.Dir(c)] = true
		}
	}
	for _, c := range copies {
		if !current[filepath.Dir(c)] || manifestVersion(c) == want || now.Sub(unusedSince(c)) < PluginGrace {
			continue
		}
		if e := removeCopy(c); e != nil {
			err = errors.Join(err, e)
			continue
		}
		removed = append(removed, c)
	}
	return removed, err
}
