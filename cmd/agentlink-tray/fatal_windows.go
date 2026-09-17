package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// fatal shows the error in a message box: a GUI executable has no console.
func fatal(err error) {
	text, _ := windows.UTF16PtrFromString(err.Error())
	title, _ := windows.UTF16PtrFromString("agentlink")
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
	os.Exit(1)
}
