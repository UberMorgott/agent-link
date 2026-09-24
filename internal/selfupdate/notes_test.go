package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// resetNotes forgets the cached releases list.
func resetNotes() {
	notes.mu.Lock()
	notes.at, notes.list, notes.limited = time.Time{}, nil, nil
	notes.mu.Unlock()
}

const releasesJSON = `[
 {"tag_name":"v0.7.0","name":"draft","body":"draft","draft":true},
 {"tag_name":"v0.6.0","name":"v0.6.0","body":"## What's Changed\r\n* six","published_at":"2026-09-20T10:00:00Z"},
 {"tag_name":"v0.5.1-rc1","body":"rc","prerelease":true},
 {"tag_name":"nightly","body":"not a version"},
 {"tag_name":"v0.4.0","body":"four"},
 {"tag_name":"v0.5.0","body":"five"}
]`

// fakeReleasesAPI serves releasesJSON (or a 403 rate limit while limited is
// set) on the releases list and counts the calls.
func fakeReleasesAPI(t *testing.T) (calls *atomic.Int32, limited *atomic.Bool) {
	t.Helper()
	calls, limited = &atomic.Int32{}, &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		if limited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(releasesJSON))
	}))
	t.Cleanup(srv.Close)
	saved := apiBase
	apiBase = srv.URL
	resetNotes()
	t.Cleanup(func() { apiBase = saved; resetNotes() })
	return calls, limited
}

func versions(list []Note) []string {
	out := []string{}
	for _, n := range list {
		out = append(out, n.Version)
	}
	return out
}

func TestChangelogAggregatesNewerReleases(t *testing.T) {
	calls, _ := fakeReleasesAPI(t)
	for _, c := range []struct {
		current string
		want    string
		newer   bool
	}{
		{"0.4.0", "[0.6.0 0.5.0]", true},
		{"0.5.0", "[0.6.0]", true},
		{"0.6.0", "[0.6.0]", false}, // up to date: the notes of this version
		{"dev", "[0.6.0]", false},   // no version: the latest notes
		{"0.9.0", "[]", false},      // newer than anything published
	} {
		list, newer, err := Changelog(context.Background(), c.current)
		if err != nil || newer != c.newer || fmt.Sprint(versions(list)) != c.want {
			t.Errorf("Changelog(%s) = %v newer=%v err=%v, want %s newer=%v", c.current, versions(list), newer, err, c.want, c.newer)
		}
	}
	list, _, _ := Changelog(context.Background(), "0.5.0")
	if list[0].Body != "## What's Changed\n* six" || list[0].Published != "2026-09-20T10:00:00Z" {
		t.Fatalf("note = %+v", list[0])
	}
	if calls.Load() != 1 {
		t.Fatalf("%d API calls, want 1 (cached)", calls.Load())
	}
}

// A rate limit without a cached list is a *RateLimitError, and later requests
// wait for the reset instead of asking again; with a cached list, that list
// is used.
func TestChangelogRateLimited(t *testing.T) {
	calls, limited := fakeReleasesAPI(t)
	limited.Store(true)
	for range 2 {
		if _, _, err := Changelog(context.Background(), "0.4.0"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("Changelog = %v, want a rate limit", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d API calls while limited, want 1", calls.Load())
	}

	resetNotes()
	limited.Store(false)
	if _, _, err := Changelog(context.Background(), "0.4.0"); err != nil {
		t.Fatal(err)
	}
	limited.Store(true)
	notes.mu.Lock()
	notes.at = time.Now().Add(-2 * notesTTL) // expired
	notes.mu.Unlock()
	list, newer, err := Changelog(context.Background(), "0.4.0")
	if err != nil || !newer || len(list) != 2 {
		t.Fatalf("rate-limited with a cache: %v %v %v", versions(list), newer, err)
	}
}

// Install reports the download as it arrives, up to the asset's size.
func TestInstallReportsProgress(t *testing.T) {
	fakeRelease(t, "v0.5.0")
	exe := install(t)
	var dones []int64
	var total int64
	if _, err := latest(t).Install(context.Background(), exe, func(done, tot int64) {
		dones = append(dones, done)
		total = tot
	}); err != nil {
		t.Fatal(err)
	}
	if len(dones) < 2 || dones[0] != 0 || dones[len(dones)-1] != int64(len(newBin)) || total != int64(len(newBin)) {
		t.Fatalf("progress %v of %d, want 0..%d", dones, total, len(newBin))
	}
	if read(t, exe) != string(newBin) {
		t.Fatal("not installed")
	}
}
