package humantime

import (
	"testing"
	"time"
)

// The same moment reads in each reader's own zone, always with its offset,
// and with the date when it is not the reader's today.
func TestFormatIn(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	nyc := time.FixedZone("EST", -5*3600)
	ist := time.FixedZone("IST", 5*3600+1800)
	at := time.Date(2026, 9, 30, 10, 51, 0, 0, time.UTC)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		t, now time.Time
		loc    *time.Location
		want   string
	}{
		{"utc+3 today", at, now, msk, "13:51 UTC+3"},
		{"utc-5 today", at, now, nyc, "05:51 UTC-5"},
		{"utc", at, now, time.UTC, "10:51 UTC"},
		{"half hour", at, now, ist, "16:21 UTC+5:30"},
		{"utc+3 yesterday", at.Add(-24 * time.Hour), now, msk, "2026-09-29 13:51 UTC+3"},
		// 02:00Z is still the 29th in UTC-5 while now is the 30th there.
		{"utc-5 day boundary", time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC), now, nyc, "2026-09-29 21:00 UTC-5"},
	} {
		if got := FormatIn(c.t, c.now, c.loc); got != c.want {
			t.Errorf("%s: FormatIn = %q, want %q", c.name, got, c.want)
		}
	}
}
