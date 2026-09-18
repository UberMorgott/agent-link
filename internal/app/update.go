package app

import (
	"context"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Release is a published version this app can install; *selfupdate.Release
// in production.
type Release interface {
	Version() string
	Apply(ctx context.Context, exePath string) ([]string, error)
}

// UpdateStatus is the update section of the settings page and the tray menu.
type UpdateStatus struct {
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`
	// Available: Latest is newer than Current and can be installed.
	Available bool `json:"available"`
	Busy      bool `json:"busy"`
	// Restarting: the new version is installed and the app is restarting.
	Restarting bool `json:"restarting,omitempty"`
	Auto       bool `json:"auto"`
	// Enabled: this build has a version and knows its executable.
	Enabled bool   `json:"enabled"`
	Text    string `json:"text,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

// Update check timing: the first check shortly after start, then every
// updateEvery with ±10% jitter so machines do not poll in step.
const (
	updateFirst = time.Minute
	updateEvery = 6 * time.Hour
)

type updater struct {
	mu      sync.Mutex
	rel     Release // newer release found by the last check, if any
	latest  string
	state   string // "", "check", "apply", "restart"
	text    string
	failed  bool
	exeStat os.FileInfo // the executable as it was at start
}

func latestRelease(ctx context.Context, current string) (Release, bool, error) {
	rel, newer, err := selfupdate.Check(ctx, current)
	if rel == nil {
		return nil, false, err
	}
	return rel, newer, err
}

// SetExecutable names this program's executable, the one an update replaces
// and Relaunch starts again. Call it before anything can replace the file.
func (a *App) SetExecutable(path string) {
	a.Exe = path
	st, err := os.Stat(path)
	if err != nil {
		st = nil
	}
	a.upd.mu.Lock()
	a.upd.exeStat = st
	a.upd.mu.Unlock()
}

func (a *App) updatesEnabled() bool {
	return selfupdate.Valid(a.Version) && a.Exe != "" && a.Relaunch != nil && a.Latest != nil
}

// UpdateStatus returns the update state.
func (a *App) UpdateStatus() UpdateStatus {
	auto := a.Settings().AutoUpdateOn()
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	return a.updateStatusLocked(auto)
}

func (a *App) updateStatusLocked(auto bool) UpdateStatus {
	st := UpdateStatus{
		Current: a.Version, Latest: a.upd.latest, Available: a.upd.rel != nil && a.upd.state == "",
		Busy: a.upd.state != "", Restarting: a.upd.state == "restart", Auto: auto,
		Enabled: a.updatesEnabled(), Text: a.upd.text, Failed: a.upd.failed,
	}
	if !st.Enabled {
		st.Text, st.Available = msg("update.disabled", nil), false
	}
	return st
}

// begin marks an update step as running; false when another one runs.
func (a *App) begin(state, text string) bool {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	if a.upd.state != "" {
		return false
	}
	a.upd.state, a.upd.text, a.upd.failed = state, text, false
	return true
}

// CheckUpdate asks GitHub for the latest release.
func (a *App) CheckUpdate(ctx context.Context) UpdateStatus {
	if !a.updatesEnabled() || !a.begin("check", msg("update.checking", nil)) {
		return a.UpdateStatus()
	}
	rel, newer, err := a.Latest(ctx, a.Version)
	a.upd.mu.Lock()
	a.upd.state, a.upd.rel, a.upd.latest = "", nil, ""
	switch {
	case err != nil:
		a.log.Warn("update check", "err", err)
		a.upd.text, a.upd.failed = msg("update.error.check", nil), true
	case rel == nil:
		a.upd.text = msg("update.none", nil)
	case newer:
		a.upd.rel, a.upd.latest = rel, rel.Version()
		a.upd.text = msg("update.available", map[string]string{"version": rel.Version()})
	default:
		a.upd.latest = rel.Version()
		a.upd.text = msg("update.latest", nil)
	}
	a.upd.mu.Unlock()
	return a.UpdateStatus()
}

// InstallUpdate installs the newer release found by a check (checking first
// if needed) and restarts the app: Relaunch starts the new executable, which
// waits for this one to quit the normal way. Running agent jobs are detached,
// so they keep running and the new app picks them up.
func (a *App) InstallUpdate(ctx context.Context) UpdateStatus {
	if !a.updatesEnabled() {
		return a.UpdateStatus()
	}
	a.upd.mu.Lock()
	rel := a.upd.rel
	a.upd.mu.Unlock()
	if rel == nil {
		if st := a.CheckUpdate(ctx); !st.Available {
			return st
		}
		a.upd.mu.Lock()
		rel = a.upd.rel
		a.upd.mu.Unlock()
	}
	vars := map[string]string{"version": rel.Version()}
	if !a.begin("apply", msg("update.applying", vars)) {
		return a.UpdateStatus()
	}
	var err error
	if a.exeReplaced() {
		// `agentlink update` already put a new version in place.
		a.log.Info("update: executable already replaced, restarting")
	} else {
		var paths []string
		paths, err = rel.Apply(ctx, a.Exe)
		a.log.Info("update installed", "version", rel.Version(), "files", paths, "err", err)
	}
	a.upd.mu.Lock()
	if err != nil {
		a.log.Error("update install", "err", err)
		a.upd.state, a.upd.text, a.upd.failed = "", msg("update.error.apply", nil), true
		a.upd.mu.Unlock()
		return a.UpdateStatus()
	}
	a.upd.state, a.upd.text = "restart", msg("update.restarting", vars)
	a.upd.mu.Unlock()
	go a.restart()
	return a.UpdateStatus()
}

// restart starts the new executable and quits this app the normal way (the
// same path as the tray's Quit), never the kill path: agents keep running.
func (a *App) restart() {
	if err := a.Relaunch(); err != nil {
		a.log.Error("update relaunch", "err", err)
		a.upd.mu.Lock()
		a.upd.rel = nil
		a.upd.state, a.upd.text, a.upd.failed = "", msg("update.error.restart", nil), true
		a.upd.mu.Unlock()
		return
	}
	a.log.Info("update: relaunched, quitting")
	a.Quit()
}

// exeReplaced reports that the executable on disk is no longer the one this
// process started from.
func (a *App) exeReplaced() bool {
	a.upd.mu.Lock()
	was := a.upd.exeStat
	a.upd.mu.Unlock()
	if was == nil {
		return false
	}
	now, err := os.Stat(a.Exe)
	return err == nil && (now.Size() != was.Size() || !now.ModTime().Equal(was.ModTime()))
}

// SetAutoUpdate turns automatic updates on or off and saves it right away,
// without restarting the node.
func (a *App) SetAutoUpdate(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	s.AutoUpdate = &on
	if a.configured {
		if err := settings.Save(a.path, s); err != nil {
			return err
		}
	}
	a.s = s
	return nil
}

// RunUpdates checks for a newer release shortly after start and then every
// few hours, and installs it while automatic updates are on. It returns when
// ctx ends, or at once for a build that cannot update itself.
func (a *App) RunUpdates(ctx context.Context) {
	if !a.updatesEnabled() {
		return
	}
	first, every := a.UpdateFirst, a.UpdateEvery
	if first <= 0 {
		first = updateFirst
	}
	if every <= 0 {
		every = updateEvery
	}
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.Settings().AutoUpdateOn() && a.CheckUpdate(ctx).Available {
			if a.InstallUpdate(ctx).Restarting {
				return
			}
		}
		jitter := every / 10
		t.Reset(every - jitter + rand.N(2*jitter+1)) // #nosec G404 -- spreading polls, not a secret
	}
}
