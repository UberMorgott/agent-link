// Command agentlink-tray is the desktop agentlink: a tray icon that runs the
// node in-process, answers requests with a local agent, and opens the
// settings and inbox pages in the browser.
//
// Build as a GUI executable (no console window):
//
//	go build -ldflags "-H=windowsgui" -o bin/agentlink-tray.exe ./cmd/agentlink-tray
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/systray"

	"github.com/UberMorgott/agent-link/internal/app"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
)

//go:embed icon.ico
var icon []byte

func main() {
	if err := run(); err != nil {
		fatal(err)
	}
}

func run() error {
	defPath, err := settings.DefaultPath()
	if err != nil {
		return err
	}
	cfgPath := flag.String("config", defPath, "settings file; its folder also holds data and the log")
	apiAddr := flag.String("api", "", "loopback address of the web UI and control API (default from settings, else "+settings.DefaultAPI+")")
	noTray := flag.Bool("no-tray", false, "run without the tray icon until interrupted or quit via the API (scripts and tests)")
	idle := flag.Duration("handler-idle-timeout", 0, "fail an agent run that printed nothing this long (default 3m; scripts and tests)")
	restarted := flag.Bool(restartFlag, false, "started by an update: wait for the previous instance to release the API address")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(selfupdate.Version)
		return nil
	}
	// Captured before an update can rename the running file.
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	logw, err := openLog(filepath.Join(filepath.Dir(*cfgPath), "agentlink.log"))
	if err != nil {
		return err
	}
	defer func() { _ = logw.Close() }()
	log := slog.New(slog.NewTextHandler(logw, nil))

	a, err := app.New(*cfgPath, log)
	if err != nil {
		return err
	}
	a.Worker.IdleTimeout = *idle
	if *apiAddr != "" {
		if err := a.SetAPIAddr(*apiAddr); err != nil {
			return err
		}
	}
	// The Run entry starts the executable without flags, so autostart only
	// makes sense for the default settings file.
	if filepath.Clean(*cfgPath) != filepath.Clean(defPath) {
		a.SetAutostart = nil
	}
	quitCtx, quit := context.WithCancel(context.Background())
	defer quit()
	a.QuitFunc = systray.Quit
	if *noTray {
		a.QuitFunc = quit
	}
	ln, err := listen(quitCtx, a.APIAddr(), *restarted)
	if err != nil {
		return fmt.Errorf("agentlink is probably already running (%s is taken): %w", a.APIAddr(), err)
	}
	log.Info("start", "version", selfupdate.Version, "exe", exe)
	go cleanupUpdate(exe, log)
	a.SetExecutable(exe)
	a.Relaunch = func() error { return selfupdate.Start(exe, relaunchArgs(os.Args[1:])) }
	go a.RunUpdates(quitCtx)
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web UI", "err", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	_ = a.Start(quitCtx) // a failure is logged and shown on the settings page
	defer a.Stop()

	if *noTray {
		ctx, stop := signal.NotifyContext(quitCtx, os.Interrupt)
		defer stop()
		<-ctx.Done()
		log.Info("quit")
		return nil
	}
	if !a.Configured() {
		openBrowser(a.URL("settings"))
	}
	systray.Run(func() { onReady(a) }, nil)
	log.Info("quit")
	return nil
}

// restartFlag marks the instance an update started; it waits for the old one.
const restartFlag = "restarted"

// listen takes the API address. After an update the old instance still holds
// it while it shuts down, so a restarted instance keeps trying for a while.
func listen(ctx context.Context, addr string, restarted bool) (net.Listener, error) {
	var lc net.ListenConfig
	deadline := time.Now().Add(30 * time.Second)
	for {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err == nil || !restarted || time.Now().After(deadline) {
			return ln, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// relaunchArgs are this run's arguments for the updated executable, marked
// as a restart once.
func relaunchArgs(args []string) []string {
	out := []string{"-" + restartFlag}
	for _, a := range args {
		if strings.TrimLeft(a, "-") != restartFlag {
			out = append(out, a)
		}
	}
	return out
}

// cleanupUpdate removes the files an earlier update left next to exe. The
// replaced executables stay locked until the old instance has exited, which
// after an update is a moment after this one started.
func cleanupUpdate(exe string, log *slog.Logger) {
	var err error
	for range 60 {
		if err = selfupdate.Cleanup(exe); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	log.Warn("update leftovers", "err", err)
}

func onReady(a *app.App) {
	systray.SetIcon(icon)
	systray.SetTitle("agentlink")
	status := systray.AddMenuItem(app.Text(app.TrayStarting, nil), "")
	status.Disable()
	membersItem := systray.AddMenuItem(app.Text(app.TrayMembers, nil), "")
	memberItems := make([]*systray.MenuItem, maxTrayMembers)
	for i := range memberItems {
		memberItems[i] = membersItem.AddSubMenuItem("", "")
		memberItems[i].Disable()
		memberItems[i].Hide()
	}
	systray.AddSeparator()
	settingsItem := systray.AddMenuItem(app.Text(app.TrayOpenSettings, nil), "")
	inboxItem := systray.AddMenuItem(app.Text(app.TrayOpenInbox, nil), "")
	systray.AddSeparator()
	versionItem := systray.AddMenuItem(app.Text("update.version", map[string]string{"version": a.Version}), "")
	versionItem.Disable()
	updText := systray.AddMenuItem("", "")
	updText.Disable()
	checkItem := systray.AddMenuItem(app.Text("update.check", nil), "")
	applyItem := systray.AddMenuItem("", "")
	autoItem := systray.AddMenuItemCheckbox(app.Text("update.auto", nil), "", a.UpdateStatus().Auto)
	systray.AddSeparator()
	quit := systray.AddMenuItem(app.Text(app.TrayQuit, nil), "")

	refresh := func() {
		st := a.Status()
		text := st.Summary()
		status.SetTitle(text)
		systray.SetTooltip("agentlink: " + text)
		showMembers(st, membersItem, memberItems)
		showUpdate(a.UpdateStatus(), updText, checkItem, applyItem, autoItem)
	}
	refresh()
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				refresh()
			case <-settingsItem.ClickedCh:
				openBrowser(a.URL("settings"))
			case <-inboxItem.ClickedCh:
				openBrowser(a.URL("inbox"))
			case <-checkItem.ClickedCh:
				go func() { a.CheckUpdate(context.Background()); refresh() }()
			case <-applyItem.ClickedCh:
				go func() { a.InstallUpdate(context.Background()); refresh() }()
			case <-autoItem.ClickedCh:
				_ = a.SetAutoUpdate(!autoItem.Checked())
				refresh()
			case <-quit.ClickedCh:
				a.Quit()
				return
			}
		}
	}()
}

// showUpdate mirrors the settings page's update section in the menu.
func showUpdate(u app.UpdateStatus, text, check, apply, auto *systray.MenuItem) {
	if u.Text == "" {
		text.Hide()
	} else {
		text.SetTitle(u.Text)
		text.Show()
	}
	if u.Available {
		apply.SetTitle(app.Text("update.apply", map[string]string{"version": u.Latest}))
		apply.Show()
	} else {
		apply.Hide()
	}
	for _, it := range []*systray.MenuItem{check, apply, auto} {
		if u.Enabled && !u.Busy {
			it.Enable()
		} else {
			it.Disable()
		}
	}
	if u.Auto {
		auto.Check()
	} else {
		auto.Uncheck()
	}
}

// maxTrayMembers bounds the members listed in the tray submenu.
const maxTrayMembers = 16

// showMembers lists the other members, online ones first, in the submenu.
func showMembers(st app.Status, parent *systray.MenuItem, items []*systray.MenuItem) {
	var others []string
	for _, m := range st.Members {
		if m.Self {
			continue
		}
		key := app.TrayMemberOff
		if m.Online {
			key = app.TrayMemberOn
		}
		others = append(others, app.Text(key, map[string]string{"name": m.Name}))
	}
	if len(others) == 0 {
		parent.Hide()
	} else {
		parent.Show()
	}
	for i, it := range items {
		if i < len(others) {
			it.SetTitle(others[i])
			it.Show()
		} else {
			it.Hide()
		}
	}
}

func openLog(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// openBrowser opens an http(s) URL in the default browser; anything else is
// ignored, so the opener never receives a file path or another scheme.
func openBrowser(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return
	}
	name, args := "xdg-open", []string{u.String()}
	switch runtime.GOOS {
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", u.String()}
	case "darwin":
		name = "open"
	}
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // G204: fixed opener binary; the only argument is an http(s) URL validated above
	_ = cmd.Start()
}
