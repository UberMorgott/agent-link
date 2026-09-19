// Package selfupdate replaces agentlink's executable with the latest GitHub
// release, gated on the SHA-256 digest GitHub reports for the release asset.
//
// The release pipeline (.github/workflows/release.yml, scripts/release.ps1)
// publishes one plain UPX-packed executable per platform to the public
// repository UberMorgott/agent-link. A check never calls the REST API: the
// unauthenticated API allows 60 requests an hour per IP address, shared by
// everyone behind one NAT or VPN. Instead it reads the tag from the redirect
// of github.com/<repo>/releases/latest (no rate limit of that kind). Only an
// install makes one API call, for the release by tag, whose asset list has
// the "digest" field ("sha256:<hex>") GitHub computes on upload; without it
// nothing is installed. Then the download from github.com, a SHA-256 compare
// against that digest and the rename dance that works on a running Windows
// executable.
//
// The download is verified before any file on disk is touched, and a failed
// swap puts the old executable back. Nothing here writes to stdout or stderr.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Version is this build's version without a leading "v". Release builds set it:
//
//	-ldflags "-X github.com/UberMorgott/agent-link/internal/selfupdate.Version=0.4.0"
//
// "dev" (a plain go build) is not a version: such a build never updates itself.
var Version = "dev"

// Repo is the GitHub repository releases come from.
const Repo = "UberMorgott/agent-link"

// webBase is the GitHub web root (release redirects and downloads) and apiBase
// the REST root. Vars so tests (and the e2e build, via -ldflags -X) can point
// them at a local server; production never changes them.
var (
	webBase = "https://github.com"
	apiBase = "https://api.github.com"
)

// Program is the one executable of a release: the desktop app and the CLI.
const Program = "agentlink"

// legacy are programs older releases shipped next to Program. Cleanup
// removes them: the one executable replaces them.
var legacy = []string{"agentlink-tray"}

// digestPrefix starts the asset "digest" field of the releases API.
const digestPrefix = "sha256:"

const (
	maxJSONSize  = 4 << 20   // 4 MiB
	maxAssetSize = 256 << 20 // 256 MiB, far above a ~10 MB build
)

// client bounds every request; a hung connection must not wedge the updater.
var client = &http.Client{Timeout: 5 * time.Minute}

// noRedirect is client for releases/latest, whose redirect is the answer.
var noRedirect = &http.Client{
	Timeout:       time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ErrRateLimited is what a *RateLimitError matches with errors.Is.
var ErrRateLimited = errors.New("GitHub rate limit")

// RateLimitError is GitHub refusing a request for its rate limit (the API's
// 60 requests an hour per IP without a token). Nothing was installed; Reset
// is when GitHub accepts requests again.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "GitHub rate limit exceeded for this network; retry after " + e.Reset.Local().Format("15:04")
}

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// rateLimit returns a *RateLimitError for a 403 or 429 that GitHub marks as
// a rate limit (X-RateLimit-Remaining 0 or a Retry-After), else nil. Reset
// comes from Retry-After (seconds), else X-RateLimit-Reset (unix time), else
// an hour from now: GitHub's window.
func rateLimit(r *response) error {
	if r.code != http.StatusForbidden && r.code != http.StatusTooManyRequests {
		return nil
	}
	retry := strings.TrimSpace(r.header.Get("Retry-After"))
	if retry == "" && strings.TrimSpace(r.header.Get("X-RateLimit-Remaining")) != "0" {
		return nil
	}
	now := time.Now()
	reset := now.Add(time.Hour)
	if s, err := strconv.ParseInt(retry, 10, 64); err == nil && s >= 0 {
		reset = now.Add(time.Duration(s) * time.Second)
	} else if u, err := strconv.ParseInt(strings.TrimSpace(r.header.Get("X-RateLimit-Reset")), 10, 64); err == nil && u > 0 {
		reset = time.Unix(u, 0)
	}
	return &RateLimitError{Reset: reset}
}

// rename is os.Rename; tests replace it to fail one step of the swap.
var rename = os.Rename

// Valid reports whether v is a version this package can order ("dev" is not).
func Valid(v string) bool {
	_, _, ok := parseVer(v)
	return ok
}

// AssetName is the release asset of program for goos/goarch. Windows amd64
// keeps the plain name the releases always had ("agentlink.exe"); any other
// platform is "<program>-<goos>-<goarch>[.exe]".
func AssetName(program, goos, goarch string) string {
	if goos == "windows" && goarch == "amd64" {
		return program + ".exe"
	}
	name := program + "-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// fileName is program's executable file name on this system.
func fileName(program string) string {
	if runtime.GOOS == "windows" {
		return program + ".exe"
	}
	return program
}

// OldPath is where an update parks the executable it replaced. On Windows a
// running executable cannot be deleted, only renamed aside, and it stays
// locked until its process exits, so only a later start (Cleanup) removes it.
func OldPath(exePath string) string {
	dir, base := filepath.Split(exePath)
	return filepath.Join(dir, "."+base+".old")
}

func newPath(exePath string) string {
	dir, base := filepath.Split(exePath)
	return filepath.Join(dir, "."+base+".new")
}

// Release is the latest published release. Whether it has an executable for
// this platform, and its digest, only Apply learns (from the API).
type Release struct {
	version string // without the leading "v"
	tag     string // as published, e.g. "v0.5.2"
}

// Version is the release version without a leading "v".
func (r *Release) Version() string { return r.version }

// Newer reports whether the release is strictly newer than current: the
// anti-downgrade gate. Fail-closed: an unparsable version on either side
// (a "dev" build, a garbled tag) is never newer.
func (r *Release) Newer(current string) bool { return compareVer(r.version, current) > 0 }

// Latest finds the newest non-draft, non-prerelease release from the redirect
// of github.com/<repo>/releases/latest to its tag page, without the API. A
// repository with no release is (nil, nil): nothing to update to.
func Latest(ctx context.Context) (*Release, error) {
	u := webBase + "/" + Repo + "/releases/latest"
	resp, err := httpGet(ctx, noRedirect, u, "text/html", maxJSONSize)
	if err != nil {
		return nil, err
	}
	if err := rateLimit(resp); err != nil {
		return nil, err
	}
	switch resp.code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("github releases/latest: HTTP %d, want a redirect to the release tag", resp.code)
	}
	return parseLatest(u, resp.header.Get("Location"))
}

// parseLatest reads the release from the Location of the releases/latest
// redirect, from, on the same host: ".../<repo>/releases/tag/<tag>". A
// redirect to ".../<repo>/releases" means the repository has no release.
func parseLatest(from, location string) (*Release, error) {
	base, err := url.Parse(from)
	if err != nil {
		return nil, err
	}
	loc, err := base.Parse(strings.TrimSpace(location))
	if err != nil || location == "" {
		return nil, fmt.Errorf("github releases/latest: bad redirect %q", location)
	}
	if loc.Host != base.Host {
		return nil, fmt.Errorf("github releases/latest: redirect to another host %q", location)
	}
	prefix := "/" + Repo + "/releases"
	if strings.TrimSuffix(loc.Path, "/") == prefix {
		return nil, nil
	}
	tag, ok := strings.CutPrefix(loc.Path, prefix+"/tag/")
	if !ok || !validTag(tag) {
		return nil, fmt.Errorf("github releases/latest: redirect %q is not a release tag", location)
	}
	return &Release{version: strings.TrimPrefix(tag, "v"), tag: tag}, nil
}

// validTag reports a version tag ("v0.5.2") made only of characters that are
// safe in a URL path segment.
func validTag(tag string) bool {
	if !Valid(tag) {
		return false
	}
	for _, c := range tag {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '.' && c != '-' && c != '+' {
			return false
		}
	}
	return true
}

// Check returns the latest release and whether it is newer than current.
func Check(ctx context.Context, current string) (rel *Release, newer bool, err error) {
	rel, err = Latest(ctx)
	if err != nil || rel == nil {
		return nil, false, err
	}
	return rel, rel.Newer(current), nil
}

func (r *Release) noDigest() error {
	return fmt.Errorf("release v%s has no sha256 digest for %s: refusing an unverifiable update",
		r.version, AssetName(Program, runtime.GOOS, runtime.GOARCH))
}

// digest asks the releases API (one call) for the SHA-256 digest of name in
// this release. A rate limit is a *RateLimitError; a missing asset or digest
// is an error too: nothing unverified is installed.
func (r *Release) digest(ctx context.Context, name string) (string, error) {
	resp, err := httpGet(ctx, client, apiBase+"/repos/"+Repo+"/releases/tags/"+url.PathEscape(r.tag), "application/vnd.github+json", maxJSONSize)
	if err != nil {
		return "", err
	}
	if err := rateLimit(resp); err != nil {
		return "", err
	}
	if resp.code != http.StatusOK {
		return "", fmt.Errorf("github releases API: HTTP %d", resp.code)
	}
	var gh struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(resp.body, &gh); err != nil {
		return "", fmt.Errorf("parse release JSON: %w", err)
	}
	for _, a := range gh.Assets {
		if a.Name == name {
			if d := parseDigest(a.Digest); d != "" {
				return d, nil
			}
			return "", r.noDigest()
		}
	}
	return "", fmt.Errorf("release v%s has no %s for this platform", r.version, name)
}

// Apply fetches the asset's SHA-256 digest from the API, downloads the
// release executable from github.com, verifies it and swaps it in for
// exePath, which must be an agentlink executable. It returns the replaced
// path. A rate limit, a missing or wrong digest aborts before anything on
// disk is touched.
func (r *Release) Apply(ctx context.Context, exePath string) ([]string, error) {
	if base := filepath.Base(exePath); !strings.EqualFold(base, fileName(Program)) {
		return nil, fmt.Errorf("%s is not the agentlink executable (%s)", base, fileName(Program))
	}
	name := AssetName(Program, runtime.GOOS, runtime.GOARCH)
	want, err := r.digest(ctx, name)
	if err != nil {
		return nil, err
	}
	dl := webBase + "/" + Repo + "/releases/download/" + url.PathEscape(r.tag) + "/" + name
	if err := requireHTTPS(dl); err != nil {
		return nil, err
	}
	resp, err := httpGet(ctx, client, dl, "application/octet-stream", maxAssetSize)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if err := rateLimit(resp); err != nil {
		return nil, err
	}
	if resp.code != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", name, resp.code)
	}
	sum := sha256.Sum256(resp.body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s: got sha256 %s, the release lists %s", name, got, want)
	}
	if err := replace([]file{{path: exePath, data: resp.body}}); err != nil {
		return nil, err
	}
	return []string{exePath}, nil
}

// parseDigest returns the lowercase hex of a "sha256:<hex>" asset digest, or
// "" for anything else: an absent digest or another algorithm.
func parseDigest(d string) string {
	hexSum, ok := strings.CutPrefix(strings.TrimSpace(d), digestPrefix)
	if !ok || len(hexSum) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(hexSum); err != nil {
		return ""
	}
	return strings.ToLower(hexSum)
}

type file struct {
	path string
	data []byte
}

// replace swaps every file in through the rename dance that works while an
// executable runs (Windows cannot overwrite a live .exe, but can rename it):
// write all ".<base>.new" first, then per file move the live one to
// ".<base>.old" and the new one into place. A failure puts back every file
// already swapped, so the folder never mixes versions.
func replace(files []file) error {
	cleanNew := func() {
		for _, f := range files {
			_ = os.Remove(newPath(f.path))
		}
	}
	for _, f := range files {
		mode := os.FileMode(0o755)
		if st, err := os.Stat(f.path); err == nil {
			mode = st.Mode().Perm()
		}
		np := newPath(f.path)
		err := os.WriteFile(np, f.data, mode) // #nosec G306 -- an executable must be executable
		if err == nil {
			err = os.Chmod(np, mode) // WriteFile keeps the mode of a leftover .new
		}
		if err != nil {
			cleanNew()
			return fmt.Errorf("write new %s: %w", filepath.Base(f.path), err)
		}
	}
	var done []file
	rollback := func() {
		for _, f := range slices.Backward(done) {
			_ = rename(OldPath(f.path), f.path)
		}
		cleanNew()
	}
	for _, f := range files {
		old := OldPath(f.path)
		// A leftover .old of an earlier update would block the rename; when it
		// is still locked (its process runs) the rename below reports that.
		_ = os.Remove(old)
		if err := rename(f.path, old); err != nil {
			rollback()
			return fmt.Errorf("move %s aside: %w", filepath.Base(f.path), err)
		}
		if err := rename(newPath(f.path), f.path); err != nil {
			_ = rename(old, f.path)
			rollback()
			return fmt.Errorf("install new %s: %w", filepath.Base(f.path), err)
		}
		done = append(done, f)
	}
	// A running executable stays locked: hide it until Cleanup at the next start.
	for _, f := range files {
		if err := os.Remove(OldPath(f.path)); err != nil {
			_ = hideFile(OldPath(f.path))
		}
	}
	return nil
}

// Cleanup removes what an update left next to exePath: the replaced
// executable (locked until its process exited), an unfinished ".new" file,
// and the executables of older releases (agentlink-tray) that the one
// agentlink executable replaces. It reports the first file that is still
// there, so a caller starting right after an update can retry until the old
// process is gone.
func Cleanup(exePath string) error {
	dir := filepath.Dir(exePath)
	var first error
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	for _, p := range append([]string{Program}, legacy...) {
		path := filepath.Join(dir, fileName(p))
		remove(OldPath(path))
		remove(newPath(path))
	}
	for _, p := range legacy {
		remove(filepath.Join(dir, fileName(p)))
	}
	return first
}

// LegacyName reports whether base is the file name of an executable older
// releases shipped next to agentlink (agentlink-tray.exe).
func LegacyName(base string) bool {
	for _, p := range legacy {
		if strings.EqualFold(base, fileName(p)) {
			return true
		}
	}
	return false
}

// requireHTTPS refuses a plaintext asset URL. Skipped when webBase itself is
// not https: only a test build does that.
func requireHTTPS(u string) error {
	if !strings.HasPrefix(webBase, "https://") {
		return nil
	}
	if !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("refusing non-HTTPS release asset URL %q", u)
	}
	return nil
}

// response is what httpGet read.
type response struct {
	code   int
	header http.Header
	body   []byte
}

// httpGet fetches u through c and returns the status, headers and body (at
// most limit bytes). client follows redirects, which is how a GitHub release
// download resolves to its storage host; noRedirect does not.
func httpGet(ctx context.Context, c *http.Client, u, accept string, limit int64) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "agentlink-selfupdate/"+Version) // GitHub rejects requests without one
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) == limit {
		return nil, fmt.Errorf("response from %s exceeds %d bytes", u, limit)
	}
	return &response{code: resp.StatusCode, header: resp.Header, body: body}, nil
}

// compareVer compares two "X.Y.Z[-pre][+build]" versions (a leading "v"
// optional): -1, 0 or +1, and 0 when either side is unparsable, which makes
// Newer fail closed. A prerelease sorts before its release; prerelease tags
// compare as plain strings (agentlink tags only vX.Y.Z).
func compareVer(a, b string) int {
	an, ap, aok := parseVer(a)
	bn, bp, bok := parseVer(b)
	if !aok || !bok {
		return 0
	}
	for i := range an {
		if an[i] != bn[i] {
			if an[i] > bn[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap > bp:
		return 1
	default:
		return -1
	}
}

// parseVer splits "vX.Y.Z-pre+build" into its numbers and prerelease tag; ok
// is false for anything but three non-negative decimal fields.
func parseVer(v string) (nums [3]int, pre string, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre, v = v[i+1:], v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nums, "", false
	}
	for i, p := range parts {
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return nums, "", false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nums, "", false
		}
		nums[i] = n
	}
	return nums, pre, true
}
