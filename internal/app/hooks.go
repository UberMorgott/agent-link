package app

// Folder hooks: the agent chosen in «Кто отвечает» gets the agentlink hook
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
	"github.com/UberMorgott/agent-link/internal/worker"
)

// Hook states of one folder.
const (
	HookOK       = "ok"
	HookNoFolder = "no_folder"
	HookError    = "error"
)

// HookStatus is what the settings page shows next to the working folder and
// each project. Client is "" when no agent answers (no hooks then).
type HookStatus struct {
	Client   string            `json:"client"`
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

// syncHooksLocked makes the hooks match the settings: the chosen agent's hook
// in the working folder and every project folder, and none of agentlink's
// entries left in folders (or for an agent) no longer chosen. A missing folder
// is skipped. Without HookExe the app manages no hooks.
func (a *App) syncHooksLocked() {
	if a.HookExe == "" {
		return
	}
	client := a.s.Handler
	if client != worker.HandlerClaude && client != worker.HandlerCodex {
		client = ""
	}
	want := map[string]bool{}
	if client != "" {
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
		if p.Client == client && want[p.Dir] {
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
		states[dir] = HookOK
		if changed {
			a.log.Info("folder hook installed", "file", file)
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
	a.hookClient, a.hookStates = client, states
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
	st := HookStatus{Client: a.hookClient}
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
