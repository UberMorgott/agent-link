// Package humantime formats a moment for a human or agent reader: the
// reader's local time with an explicit UTC offset, so it is never ambiguous
// next to the RFC 3339 UTC times of JSON ("13:51 UTC+3", or
// "2026-09-29 13:51 UTC+3" when it is not today). Machine data (JSON, API)
// keeps RFC 3339 UTC and never goes through here.
package humantime

import (
	"fmt"
	"time"
)

// Format is t in this machine's time zone, with the date when it is not today.
func Format(t time.Time) string { return FormatIn(t, time.Now(), time.Local) }

// FormatIn is t in loc, with the date when it is not now's day in loc.
func FormatIn(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	layout := "15:04"
	if y, m, d := t.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		layout = "2006-01-02 15:04"
	}
	return t.Format(layout) + " " + Offset(t)
}

// Offset is t's UTC offset: "UTC", "UTC+3", "UTC-5", "UTC+5:30".
func Offset(t time.Time) string {
	_, off := t.Zone()
	if off == 0 {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	if m := off % 3600 / 60; m != 0 {
		return fmt.Sprintf("UTC%s%d:%02d", sign, off/3600, m)
	}
	return fmt.Sprintf("UTC%s%d", sign, off/3600)
}
