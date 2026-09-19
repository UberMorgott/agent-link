module github.com/UberMorgott/agent-link

go 1.27

require (
	fyne.io/systray v1.12.2
	github.com/gtank/ristretto255 v0.2.0
	go.uber.org/goleak v1.3.0
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
)

replace fyne.io/systray => ./third_party/vendor/systray
