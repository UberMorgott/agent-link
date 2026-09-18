// Package selfupdate replaces agentlink's executables with the latest GitHub
// release, gated on the SHA-256 published in that release's checksums.txt.
//
// The release pipeline (.github/workflows/release.yml, scripts/release.ps1)
// publishes plain UPX-packed executables plus a `sha256sum` checksums.txt to
// the public repository UberMorgott/agent-link. So the whole job is one REST
// call without a token, an exact asset-name match, a SHA-256 compare and the
// rename dance that works on a running Windows executable.
//
// Both programs live in one folder and are replaced together: the running one
// and its sibling, when it is there. Every download is verified before any
// file on disk is touched, and a failed swap rolls back what was already
// swapped. Nothing here writes to stdout or stderr.
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
	"os"
	"path/filepath"
	"runtime"
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

// apiBase is the GitHub REST root. A var so tests (and the e2e build, via
// -ldflags -X) can point it at a local server; production never changes it.
var apiBase = "https://api.github.com"

// ChecksumsAsset lists the SHA-256 of every release executable, one
// "<hash>  <name>" line each (`sha256sum` format). A release without it is
// never applied: the update path has no other authenticity gate.
const ChecksumsAsset = "checksums.txt"

// Programs are the executables of a release, replaced together.
var Programs = []string{"agentlink", "agentlink-tray"}

const (
	maxJSONSize  = 4 << 20   // 4 MiB
	maxSumsSize  = 1 << 20   // 1 MiB
	maxAssetSize = 256 << 20 // 256 MiB, far above a ~10 MB build
)

// client bounds every request; a hung connection must not wedge the updater.
var client = &http.Client{Timeout: 5 * time.Minute}

// rename is os.Rename; tests replace it to fail one step of the swap.
var rename = os.Rename

// Valid reports whether v is a version this package can order ("dev" is not).
func Valid(v string) bool {
	_, _, ok := parseVer(v)
	return ok
}

// AssetName is the release asset of program for goos/goarch. Windows amd64
// keeps the plain names the releases always had ("agentlink.exe"); any other
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

// Release is the latest published release with executables for this platform.
type Release struct {
	version string            // without the leading "v"
	assets  map[string]string // program -> download URL
	sumsURL string
}

// Version is the release version without a leading "v".
func (r *Release) Version() string { return r.version }

// Newer reports whether the release is strictly newer than current: the
// anti-downgrade gate. Fail-closed: an unparsable version on either side
// (a "dev" build, a garbled tag) is never newer.
func (r *Release) Newer(current string) bool { return compareVer(r.version, current) > 0 }

// Latest fetches the newest non-draft, non-prerelease release. A repository
// with no release, or a release without an executable for this platform, is
// (nil, nil): nothing to update to, not a failure.
func Latest(ctx context.Context) (*Release, error) {
	body, code, err := httpGet(ctx, apiBase+"/repos/"+Repo+"/releases/latest", "application/vnd.github+json", maxJSONSize)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("github releases API: HTTP %d", code)
	}
	var gh struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &gh); err != nil {
		return nil, fmt.Errorf("parse release JSON: %w", err)
	}
	rel := &Release{version: strings.TrimPrefix(strings.TrimSpace(gh.TagName), "v"), assets: map[string]string{}}
	if !Valid(rel.version) {
		return nil, fmt.Errorf("release tag %q is not a version", gh.TagName)
	}
	for _, a := range gh.Assets {
		if a.Name == ChecksumsAsset {
			rel.sumsURL = a.URL
		}
		for _, p := range Programs {
			if a.Name == AssetName(p, runtime.GOOS, runtime.GOARCH) {
				rel.assets[p] = a.URL
			}
		}
	}
	if len(rel.assets) == 0 {
		return nil, nil
	}
	for _, u := range rel.assets {
		if err := requireHTTPS(u); err != nil {
			return nil, err
		}
	}
	if rel.sumsURL != "" {
		if err := requireHTTPS(rel.sumsURL); err != nil {
			return nil, err
		}
	}
	return rel, nil
}

// Check returns the latest release and whether it is newer than current. A
// newer release without checksums.txt is an error: it could not be applied.
func Check(ctx context.Context, current string) (rel *Release, newer bool, err error) {
	rel, err = Latest(ctx)
	if err != nil || rel == nil {
		return nil, false, err
	}
	newer = rel.Newer(current)
	if newer && rel.sumsURL == "" {
		return rel, false, fmt.Errorf("release v%s has no %s: refusing an unverifiable update", rel.version, ChecksumsAsset)
	}
	return rel, newer, nil
}

// Targets returns the executables an update of exePath replaces: exePath
// itself (it must be one of Programs) and each sibling program in its folder.
func Targets(exePath string) (map[string]string, error) {
	dir, base := filepath.Split(exePath)
	out := map[string]string{}
	for _, p := range Programs {
		name := fileName(p)
		switch {
		case strings.EqualFold(base, name):
			out[p] = exePath
		default:
			path := filepath.Join(dir, name)
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
				out[p] = path
			}
		}
	}
	for _, path := range out {
		if path == exePath {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%s is not an agentlink executable (%s)", base, strings.Join(Programs, ", "))
}

// Apply downloads the release executables for exePath's folder (Targets),
// verifies each against checksums.txt and swaps them all in. It returns the
// replaced paths. A hash that is missing or wrong aborts before anything on
// disk is touched.
func (r *Release) Apply(ctx context.Context, exePath string) ([]string, error) {
	targets, err := Targets(exePath)
	if err != nil {
		return nil, err
	}
	if r.sumsURL == "" {
		return nil, fmt.Errorf("release v%s has no %s: refusing an unverifiable update", r.version, ChecksumsAsset)
	}
	sums, code, err := httpGet(ctx, r.sumsURL, "application/octet-stream", maxSumsSize)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", ChecksumsAsset, err)
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", ChecksumsAsset, code)
	}
	var files []file
	for _, p := range Programs {
		path, ok := targets[p]
		if !ok {
			continue
		}
		name := AssetName(p, runtime.GOOS, runtime.GOARCH)
		url, ok := r.assets[p]
		if !ok {
			return nil, fmt.Errorf("release v%s has no %s", r.version, name)
		}
		want, err := sumFor(sums, name)
		if err != nil {
			return nil, err
		}
		data, code, err := httpGet(ctx, url, "application/octet-stream", maxAssetSize)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", name, err)
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("download %s: HTTP %d", name, code)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != want {
			return nil, fmt.Errorf("checksum mismatch for %s: got %s, %s lists %s", name, got, ChecksumsAsset, want)
		}
		files = append(files, file{path: path, data: data})
	}
	if err := replace(files); err != nil {
		return nil, err
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	return paths, nil
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
		for i := len(done) - 1; i >= 0; i-- {
			_ = rename(OldPath(done[i].path), done[i].path)
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

// Cleanup removes what an update left next to exePath's programs: the
// replaced executables (locked until their process exited) and unfinished
// ".new" files. It reports the first file that is still there, so a caller
// starting right after an update can retry until the old process is gone.
func Cleanup(exePath string) error {
	dir := filepath.Dir(exePath)
	var first error
	for _, p := range Programs {
		path := filepath.Join(dir, fileName(p))
		for _, leftover := range []string{OldPath(path), newPath(path)} {
			if err := os.Remove(leftover); err != nil && !errors.Is(err, fs.ErrNotExist) && first == nil {
				first = err
			}
		}
	}
	return first
}

// sumFor finds the SHA-256 of name in `sha256sum` output ("<hash>  <name>",
// or "<hash> *<name>" in binary mode). An unlisted asset is an error: it must
// never fall through to "no hash, install anyway".
func sumFor(sums []byte, name string) (string, error) {
	for line := range strings.Lines(string(sums)) {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == sha256.Size*2 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s does not list %s: refusing an unverifiable update", ChecksumsAsset, name)
}

// requireHTTPS refuses a plaintext asset URL. The URLs come from the API
// response, which is only as trustworthy as the TLS connection that delivered
// it. Skipped when apiBase itself is not https: only a test build does that.
func requireHTTPS(u string) error {
	if !strings.HasPrefix(apiBase, "https://") {
		return nil
	}
	if !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("refusing non-HTTPS release asset URL %q", u)
	}
	return nil
}

// httpGet fetches url and returns the body (at most limit bytes) with the
// status code. Redirects are followed, which is how a GitHub
// browser_download_url resolves to its storage host.
func httpGet(ctx context.Context, url, accept string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "agentlink-selfupdate/"+Version) // GitHub rejects requests without one
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(body)) == limit {
		return nil, resp.StatusCode, fmt.Errorf("response from %s exceeds %d bytes", url, limit)
	}
	return body, resp.StatusCode, nil
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
