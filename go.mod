module github.com/UberMorgott/agent-link

go 1.27

// The web UI's npm dependencies ship stray Go files; they are not part of this module.
ignore ./internal/app/web/node_modules

require (
	fyne.io/systray v1.12.2
	github.com/gtank/ristretto255 v0.2.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	go.uber.org/goleak v1.3.0
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace fyne.io/systray => ./third_party/vendor/systray
