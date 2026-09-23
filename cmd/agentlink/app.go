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
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/UberMorgott/agent-link/internal/app"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
)

//go:embed icon.ico
var icon []byte

// runApp is the desktop app: agentlink without a command. args are its flags.
func runApp(args []string) error {
	defPath, err := settings.DefaultPath()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("agentlink", flag.ExitOnError)
	cfgPath := fs.String("config", defPath, "settings file; its folder also holds data and the log")
	apiAddr := fs.String("api", "", "loopback address of the web UI and control API (default from settings, else "+settings.DefaultAPI+")")
	noTray := fs.Bool("no-tray", false, "run without the tray icon until interrupted or quit via the API (scripts and tests)")
	idle := fs.Duration("handler-idle-timeout", 0, "fail an agent run that printed nothing this long (default 3m; scripts and tests)")
	restarted := fs.Bool(restartFlag, false, "started by an update: wait for the previous instance to release the API address")
	showVersion := fs.Bool("version", false, "print the version and exit")
	_ = fs.Parse(args) // ExitOnError
	if *showVersion {
		fmt.Println(selfupdate.Version)
		return nil
	}
	// The tray app needs no console: leave the one Windows opened for a
	// double-click, or let the terminal that started it go on.
	if !*noTray && leaveConsole(args) {
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
		a.SetAutostart, a.AutostartState = nil, nil
	} else if moved, err := app.MigrateAutostart(exe); err != nil {
		log.Warn("autostart migration", "err", err)
	} else if moved {
		log.Info("autostart now starts this executable", "exe", exe)
	}
	// Folder hooks too: `agentlink hook` reads the default settings file.
	if filepath.Clean(*cfgPath) == filepath.Clean(defPath) {
		a.HookExe = exe
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
	a.Relaunch = func() error { return selfupdate.Start(exe, relaunchArgs(args)) }
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
		openBrowser(startupURL(a, false))
	}
	clicks := &debounce{gap: clickGap}
	systray.SetOnTapped(func() { // left click; a right click shows the menu
		if clicks.allow(time.Now()) {
			openBrowser(dashboardURL(a))
		}
	})
	autostartChanged := make(chan struct{}, 1)
	a.AutostartChanged = func(bool) {
		select {
		case autostartChanged <- struct{}{}:
		default: // one pending refresh reads the latest state anyway
		}
	}
	systray.Run(func() { onReady(a, log, autostartChanged) }, nil)
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

// onReady builds the tray menu once. It never changes while it may be open:
// Windows redraws (and on some builds closes) a popup menu whose items are
// modified under TrackPopupMenu, so live state goes to the tooltip instead
// and the checkbox changes only in response to a click or a settings save.
func onReady(a *app.App, log *slog.Logger, autostartChanged <-chan struct{}) {
	systray.SetIcon(icon)
	systray.SetTitle("agentlink")
	on, available := a.Autostart()
	autostart := systray.AddMenuItemCheckbox(app.Text(app.TrayAutostart, nil), "", on)
	if !available {
		autostart.Disable()
	}
	open := systray.AddMenuItem(app.Text(app.TrayOpenBrowser, nil), "")
	systray.AddSeparator()
	quit := systray.AddMenuItem(app.Text(app.TrayQuit, nil), "")

	var tip string
	refresh := func() {
		if t := app.TrayTooltip(a.Status(), a.UpdateStatus()); t != tip {
			tip = t
			systray.SetTooltip(t)
		}
	}
	refresh()
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				refresh()
			case <-autostart.ClickedCh:
				if err := a.SetAutostartNow(!autostart.Checked()); err != nil {
					log.Warn("autostart", "err", err)
				}
				on, _ := a.Autostart()
				setChecked(autostart, on)
			case <-autostartChanged: // saved on the settings page
				on, _ := a.Autostart()
				setChecked(autostart, on)
			case <-open.ClickedCh:
				openBrowser(dashboardURL(a))
			case <-quit.ClickedCh:
				a.Quit()
				return
			}
		}
	}()
}

func setChecked(it *systray.MenuItem, on bool) {
	if on {
		it.Check()
	} else {
		it.Uncheck()
	}
}

// clickGap is the debounce of a left click on the tray icon: a double click
// sends WM_LBUTTONUP twice, and should still open one browser tab.
const clickGap = time.Second

// debounce passes the first call and drops any that follow within gap of
// the last one passed.
type debounce struct {
	gap  time.Duration
	mu   sync.Mutex
	last time.Time
}

func (d *debounce) allow(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.last.IsZero() && now.Sub(d.last) < d.gap {
		return false
	}
	d.last = now
	return true
}

func dashboardURL(a *app.App) string { return a.URL("open") }

func startupURL(a *app.App, configured bool) string {
	if !configured {
		return a.URL("settings")
	}
	return dashboardURL(a)
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
