package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// fatal reports an error of the desktop app and exits: on the console while
// it has one, else in a message box.
func fatal(err error) {
	if !gui {
		_, _ = fmt.Fprintln(os.Stderr, "agentlink:", err)
		os.Exit(1)
	}
	text, _ := windows.UTF16PtrFromString(err.Error())
	title, _ := windows.UTF16PtrFromString("agentlink")
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
	os.Exit(1)
}
