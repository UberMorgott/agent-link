package app

// Folder hooks: each available agent gets the agentlink hook
// (`agentlink hook claude|codex`) in the working folder and in every project
// folder, so a live session opened there hears of new messages. The hook
// itself picks the messages of its folder (see the hook command).

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// Hook states of one folder.
const (
	HookOK       = "ok"
	HookNoFolder = "no_folder"
	HookError    = "error"
)

// HookStatus is what the settings page shows next to the working folder and
// each project. Client is "" when no folder hook is needed, or "both" when
// both providers have one.
type HookStatus struct {
	Client   string            `json:"client"`
	Clients  []string          `json:"clients,omitempty"`
	WorkDir  string            `json:"work_dir,omitempty"`
	Projects map[string]string `json:"projects,omitempty"`
}

// installedHook is a folder this app put the hook of client into; the list is
// kept so a changed folder or agent takes the old entries out again.
type installedHook struct {
	Dir    string `json:"dir"`
	Client string `json:"client"`
}

func (a *App) installedHooksPath() string {
	return filepath.Join(filepath.Dir(a.path), "folder-hooks.json")
}

// syncHooksLocked installs the hooks of both configured providers in the
// working and project folders. A provider's plugin replaces its folder hook
// to avoid duplicate delivery. Old entries are removed when no longer needed.
func (a *App) syncHooksLocked() {
	if a.HookExe == "" {
		return
	}
	var clients []string
	for _, provider := range []string{worker.HandlerClaude, worker.HandlerCodex} {
		available := a.s.Handler == provider || a.s.ProgramPath(provider) != ""
		if !available && a.Agents.LookPath != nil {
			_, source := a.Agents.Resolve(provider, "")
			available = source != "" && source != settings.AgentMissing
		}
		if available && !agenthook.PluginEnabled(provider) {
			clients = append(clients, provider)
		}
	}
	want := map[string]bool{}
	if len(clients) > 0 {
		for _, dir := range append([]string{a.s.WorkDir}, a.projectDirs()...) {
			if dir != "" {
				want[filepath.Clean(dir)] = true
			}
		}
	}
	var prev, kept []installedHook
	data, err := os.ReadFile(filepath.Clean(a.installedHooksPath()))
	if err == nil {
		err = json.Unmarshal(data, &prev)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.log.Warn("folder hooks list", "err", err)
	}
	for _, p := range prev {
		if slices.Contains(clients, p.Client) && want[p.Dir] {
			continue // installed again below
		}
		if _, err := agenthook.Remove(agenthook.ProjectFile(p.Dir, p.Client), p.Client); err != nil {
			a.log.Warn("remove folder hook", "dir", p.Dir, "client", p.Client, "err", err)
			kept = append(kept, p) // tried again next time
		}
	}
	states := map[string]string{}
	for dir := range want {
		if !agenthook.IsDir(dir) {
			states[dir] = HookNoFolder
			continue
		}
		states[dir] = HookOK
		for _, client := range clients {
			file := agenthook.ProjectFile(dir, client)
			kept = append(kept, installedHook{Dir: dir, Client: client})
			changed, err := agenthook.Install(file, client, a.HookExe)
			if err == nil {
				err = agenthook.ExcludeFromGit(dir, file)
			}
			if err != nil {
				states[dir] = HookError
				a.log.Warn("install folder hook", "dir", dir, "client", client, "err", err)
				continue
			}
			if changed {
				a.log.Info("folder hook installed", "file", file)
			}
		}
	}
	slices.SortFunc(kept, func(x, y installedHook) int {
		if c := cmp.Compare(x.Dir, y.Dir); c != 0 {
			return c
		}
		return cmp.Compare(x.Client, y.Client)
	})
	kept = slices.Compact(kept)
	if err := writeJSONFile(a.installedHooksPath(), kept); err != nil {
		a.log.Warn("save folder hooks list", "err", err)
	}
	client := ""
	if len(clients) == 1 {
		client = clients[0]
	} else if len(clients) == 2 {
		client = "both"
	}
	a.hookClient, a.hookClients, a.hookStates = client, clients, states
}

func (a *App) projectDirs() []string {
	var dirs []string
	for _, p := range a.s.Projects {
		dirs = append(dirs, p.Dir)
	}
	for _, b := range a.s.Bindings {
		dirs = append(dirs, b.Dir) // "" (no folder) is skipped
	}
	return dirs
}

// HookStatus reports the hooks of the last sync for the current folders.
func (a *App) HookStatus() HookStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := HookStatus{Client: a.hookClient, Clients: slices.Clone(a.hookClients)}
	if st.Client == "" {
		return st
	}
	state := func(dir string) string {
		if dir == "" {
			return ""
		}
		return a.hookStates[filepath.Clean(dir)]
	}
	st.WorkDir = state(a.s.WorkDir)
	for area, p := range a.s.Projects {
		if st.Projects == nil {
			st.Projects = map[string]string{}
		}
		st.Projects[area] = state(p.Dir)
	}
	return st
}

// writeJSONFile writes v to path through a temporary file.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
