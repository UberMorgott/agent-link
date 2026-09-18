//go:build !windows

package app

// pickFolder has no native dialog outside Windows; the page falls back to typing.
func pickFolder(string, string) (string, error) { return "", ErrPickUnsupported }
