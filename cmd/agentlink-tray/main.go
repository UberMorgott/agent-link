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
	ln, err := listen(a.APIAddr(), *restarted)
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

	_ = a.Start() // a failure is logged and shown on the settings page
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
func listen(addr string, restarted bool) (net.Listener, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
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
	status := systray.AddMenuItem("Starting...", "")
	status.Disable()
	systray.AddSeparator()
	settingsItem := systray.AddMenuItem("Open settings", "")
	inboxItem := systray.AddMenuItem("Open inbox", "")
	systray.AddSeparator()
	versionItem := systray.AddMenuItem(app.Text("update.version", map[string]string{"version": a.Version}), "")
	versionItem.Disable()
	updText := systray.AddMenuItem("", "")
	updText.Disable()
	checkItem := systray.AddMenuItem(app.Text("update.check", nil), "")
	applyItem := systray.AddMenuItem("", "")
	autoItem := systray.AddMenuItemCheckbox(app.Text("update.auto", nil), "", a.UpdateStatus().Auto)
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "")

	refresh := func() {
		text := statusText(a.Status())
		status.SetTitle(text)
		systray.SetTooltip("agentlink: " + text)
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

func statusText(s app.Status) string {
	switch {
	case !s.Configured:
		return "Not set up - open settings"
	case s.Error != "":
		return "Error - open settings"
	case s.Connected:
		return s.Peer + " connected"
	case s.Problem != "" || s.Peer == "":
		return "Not connected - open settings"
	default:
		return s.Peer + " offline"
	}
}

func openLog(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
