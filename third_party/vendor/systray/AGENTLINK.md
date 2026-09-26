# fyne.io/systray v1.12.2, patched for agent-link

A copy of `fyne.io/systray@v1.12.2` (module cache, without `example/` and the upstream tests: `systray_windows_test.go` does not compile against v1.12.2 itself), used through
`replace fyne.io/systray => ./third_party/vendor/systray` in the root `go.mod`. It is a
separate module under a `vendor` directory: `./...` in the root, qgate's stack detection and
typos (`_typos.toml`) leave it alone, as somebody else's code.

Four files differ from upstream, `systray.go`, `systray_windows.go`, `systray_unix.go` and
`systray_darwin.go`; the exact change is `agentlink.patch` (`git diff --no-index` of each
against `<modcache>/fyne.io/systray@v1.12.2/`):

- `ShowNotification(title, text)` shows a notification balloon from the tray icon
  (`NIF_INFO`, cleared again so later icon or tooltip changes do not repeat it) and
  `SetOnNotificationTapped(f)` runs `f` on a click on it (`NIN_BALLOONUSERCLICK`). Windows
  only; `ShowNotification` is a no-op on the other systems.

- The tray icon is loaded at the notification-area size for the window's DPI
  (`GetSystemMetricsForDpi(SM_CXSMICON/SM_CYSMICON, GetDpiForWindow)`, falling back to
  `GetSystemMetrics`) instead of `LR_DEFAULTSIZE`, which loads the large `SM_CXICON` image
  and lets the shell shrink it into a blurry icon. Loaded handles are cached per size.
- On `WM_DPICHANGED`, `WM_DISPLAYCHANGE` and `TaskbarCreated` the icon is loaded again at
  the then-current size.

To move to a newer systray: copy the new version here the same way, re-apply
`agentlink.patch`, and drop the copy if upstream loads the icon per DPI by itself.
