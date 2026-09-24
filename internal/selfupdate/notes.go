package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Note is one published release in the changelog: its notes as GitHub keeps
// them (Markdown, shown as plain text).
type Note struct {
	Version   string `json:"version"` // without the leading "v"
	Name      string `json:"name,omitempty"`
	Body      string `json:"body"`
	Published string `json:"published,omitempty"` // RFC 3339
}

// notesTTL is how long one releases list serves every changelog request.
const notesTTL = 10 * time.Minute

// notes caches the releases list, and while GitHub rate-limits this network,
// the limit itself, so reopening the changelog spends no request of the
// unauthenticated 60 an hour.
var notes struct {
	mu      sync.Mutex
	at      time.Time // when list was fetched; zero: never
	list    []Note    // newest first
	limited *RateLimitError
}

// Changelog returns the notes of every stable release newer than current, up
// to the latest, newest first, and newer true. With nothing newer it is the
// notes of current itself; for a build without a version ("dev"), of the
// latest release. One API call lists the releases; its answer is cached for
// notesTTL. When GitHub fails or rate-limits a later call, the cached list
// is used; without one, a rate limit is a *RateLimitError.
func Changelog(ctx context.Context, current string) (list []Note, newer bool, err error) {
	all, err := releases(ctx)
	if err != nil {
		return nil, false, err
	}
	if !Valid(current) {
		if len(all) == 0 {
			return nil, false, nil
		}
		return all[:1], false, nil
	}
	for _, n := range all {
		switch c := compareVer(n.Version, current); {
		case c > 0:
			list = append(list, n)
		case c == 0 && len(list) == 0:
			return []Note{n}, false, nil
		}
	}
	return list, len(list) > 0, nil
}

// releases is the cached list of stable releases, newest first.
func releases(ctx context.Context) ([]Note, error) {
	notes.mu.Lock()
	defer notes.mu.Unlock()
	now := time.Now()
	if !notes.at.IsZero() && now.Sub(notes.at) < notesTTL {
		return notes.list, nil
	}
	if notes.limited != nil && now.Before(notes.limited.Reset) {
		if !notes.at.IsZero() {
			return notes.list, nil
		}
		return nil, notes.limited
	}
	list, err := fetchReleases(ctx)
	if err != nil {
		if rl, ok := errors.AsType[*RateLimitError](err); ok {
			notes.limited = rl
		}
		if !notes.at.IsZero() {
			return notes.list, nil
		}
		return nil, err
	}
	notes.at, notes.list, notes.limited = now, list, nil
	return list, nil
}

// fetchReleases asks the releases API (one call, the newest 100) for every
// published, non-prerelease release with a version tag.
func fetchReleases(ctx context.Context) ([]Note, error) {
	resp, err := httpGet(ctx, client, apiBase+"/repos/"+Repo+"/releases?per_page=100", "application/vnd.github+json", maxJSONSize)
	if err != nil {
		return nil, err
	}
	if err := rateLimit(resp); err != nil {
		return nil, err
	}
	if resp.code != http.StatusOK {
		return nil, fmt.Errorf("github releases API: HTTP %d", resp.code)
	}
	var gh []struct {
		Tag        string `json:"tag_name"`
		Name       string `json:"name"`
		Body       string `json:"body"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Published  string `json:"published_at"`
	}
	if err := json.Unmarshal(resp.body, &gh); err != nil {
		return nil, fmt.Errorf("parse releases JSON: %w", err)
	}
	list := []Note{}
	for _, r := range gh {
		if r.Draft || r.Prerelease || !validTag(r.Tag) {
			continue
		}
		list = append(list, Note{
			Version: strings.TrimPrefix(r.Tag, "v"), Name: r.Name,
			Body: strings.TrimSpace(strings.ReplaceAll(r.Body, "\r\n", "\n")), Published: r.Published,
		})
	}
	slices.SortStableFunc(list, func(a, b Note) int { return compareVer(b.Version, a.Version) })
	return list, nil
}
