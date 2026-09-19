package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// digestOf is the releases API "digest" of b.
func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(h[:])
}

// fake is a fake github.com and api.github.com in one server, which webBase
// and apiBase point at for the test.
type fake struct {
	url      string
	location string      // releases/latest redirect ("": answer 200, no redirect)
	status   int         // releases/latest status (0: 302)
	bin      []byte      // this platform's asset (nil: no such asset)
	digest   string      // its "digest" field ("": none)
	limited  http.Header // non-nil: the API answers 403 with these headers
	apiCalls atomic.Int32
}

// fakeGitHub serves one release with tag whose asset for this platform is bin
// with the given digest.
func fakeGitHub(t *testing.T, tag string, bin []byte, digest string) *fake {
	t.Helper()
	f := &fake{location: "/" + Repo + "/releases/tag/" + tag, bin: bin, digest: digest}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+Repo+"/releases/latest":
			if f.location == "" {
				_, _ = w.Write([]byte("<html>no redirect</html>"))
				return
			}
			code := f.status
			if code == 0 {
				code = http.StatusFound
			}
			w.Header().Set("Location", f.location)
			w.WriteHeader(code)
		case r.URL.Path == "/repos/"+Repo+"/releases/tags/"+tag:
			f.apiCalls.Add(1)
			if f.limited != nil {
				maps.Copy(w.Header(), f.limited)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			list := []string{fmt.Sprintf(`{"name":"someone-else","digest":%q}`, digestOf([]byte("other")))}
			if f.bin != nil {
				d := ""
				if f.digest != "" {
					d = fmt.Sprintf(`,"digest":%q`, f.digest)
				}
				list = append(list, fmt.Sprintf(`{"name":%q%s}`, AssetName(Program, runtime.GOOS, runtime.GOARCH), d))
			}
			_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[%s]}`, tag, strings.Join(list, ","))
		case r.URL.Path == "/"+Repo+"/releases/download/"+tag+"/"+AssetName(Program, runtime.GOOS, runtime.GOARCH) && f.bin != nil:
			_, _ = w.Write(f.bin)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	savedWeb, savedAPI := webBase, apiBase
	webBase, apiBase = srv.URL, srv.URL
	t.Cleanup(func() { webBase, apiBase = savedWeb, savedAPI })
	return f
}

var newBin = []byte("new agentlink")

// fakeRelease serves newBin as tag with its honest digest.
func fakeRelease(t *testing.T, tag string) *fake { return fakeGitHub(t, tag, newBin, digestOf(newBin)) }

// install writes the old program into a folder and returns its path.
func install(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), fileName(Program))
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil { // #nosec G306
		t.Fatal(err)
	}
	return exe
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- a test temp file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAssetNames(t *testing.T) {
	cases := map[[3]string]string{
		{"agentlink", "windows", "amd64"}: "agentlink.exe",
		{"agentlink", "linux", "amd64"}:   "agentlink-linux-amd64",
		{"agentlink", "windows", "arm64"}: "agentlink-windows-arm64.exe",
	}
	for in, want := range cases {
		if got := AssetName(in[0], in[1], in[2]); got != want {
			t.Errorf("AssetName%v = %q, want %q", in, got, want)
		}
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		rel, cur string
		want     bool
	}{
		{"0.4.1", "0.4.0", true},
		{"0.10.0", "0.9.0", true},
		{"1.0.0", "0.99.99", true},
		{"0.4.0", "0.4.0", false},
		{"0.3.0", "0.4.0", false},
		{"0.9.0", "0.10.0", false},
		{"0.4.0-rc1", "0.4.0", false},
		{"0.4.0", "0.4.0-rc1", true},
		{"0.4.0+b7", "0.4.0", false},
		{"0.4.1", "dev", false}, // a dev build never updates
		{"0.4.1", "", false},
		{"garbage", "0.4.0", false},
		{"0.4", "0.3.0", false},
		{"0.4.+1", "0.3.0", false},
	}
	for _, c := range cases {
		if got := (&Release{version: c.rel}).Newer(c.cur); got != c.want {
			t.Errorf("Release(%q).Newer(%q) = %v, want %v", c.rel, c.cur, got, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		name, tag, cur string
		newer          bool
	}{
		{"newer", "v0.5.0", "0.4.0", true},
		{"older", "v0.3.0", "0.4.0", false},
		{"equal", "v0.4.0", "0.4.0", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeRelease(t, c.tag)
			rel, newer, err := Check(context.Background(), c.cur)
			if err != nil || rel == nil {
				t.Fatalf("Check = %v, %v", rel, err)
			}
			if newer != c.newer || rel.Version() != strings.TrimPrefix(c.tag, "v") {
				t.Fatalf("Check = %s newer=%v, want newer=%v", rel.Version(), newer, c.newer)
			}
		})
	}
}

func TestLatestRedirect(t *testing.T) {
	for _, c := range []struct {
		name, location string
		status         int
		version        string // "": no release
		fails          bool
	}{
		{"relative", "/" + Repo + "/releases/tag/v0.5.2", 0, "0.5.2", false},
		{"absolute", "{srv}/" + Repo + "/releases/tag/v1.10.0", 0, "1.10.0", false},
		{"moved permanently", "/" + Repo + "/releases/tag/v0.6.0", http.StatusMovedPermanently, "0.6.0", false},
		{"no release yet", "/" + Repo + "/releases", 0, "", false},
		{"not a version", "/" + Repo + "/releases/tag/nightly", 0, "", true},
		{"escaped slash", "/" + Repo + "/releases/tag/v0.5.2-a%2F..", 0, "", true},
		{"another repo", "/someone/else/releases/tag/v0.5.2", 0, "", true},
		{"another host", "https://evil.example/" + Repo + "/releases/tag/v0.5.2", 0, "", true},
		{"empty location", " ", 0, "", true},
		{"no redirect", "", 0, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := fakeGitHub(t, "v0.0.0", newBin, digestOf(newBin))
			f.location, f.status = strings.ReplaceAll(c.location, "{srv}", f.url), c.status
			rel, err := Latest(context.Background())
			switch {
			case c.fails:
				if err == nil {
					t.Fatalf("Latest = %+v, want an error", rel)
				}
			case err != nil:
				t.Fatal(err)
			case c.version == "" && rel != nil, c.version != "" && (rel == nil || rel.Version() != c.version):
				t.Fatalf("Latest = %+v, want version %q", rel, c.version)
			}
			if f.apiCalls.Load() != 0 {
				t.Fatalf("a check called the API %d times", f.apiCalls.Load())
			}
		})
	}
}

func TestLatestNothingToUpdateTo(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	saved := webBase
	webBase = srv.URL
	defer func() { webBase = saved }()
	if rel, err := Latest(context.Background()); rel != nil || err != nil {
		t.Fatalf("no release: %v, %v", rel, err)
	}
}

// A release without an executable for this platform cannot be installed.
func TestApplyRefusesMissingAsset(t *testing.T) {
	fakeGitHub(t, "v0.5.0", nil, "")
	exe := install(t)
	if _, err := latest(t).Apply(context.Background(), exe); err == nil || !strings.Contains(err.Error(), "for this platform") {
		t.Fatalf("Apply = %v, want a missing-asset refusal", err)
	}
	assertUntouched(t, exe)
}

// A rate-limited API on install is a *RateLimitError with GitHub's reset
// time, and nothing is installed.
func TestApplyRateLimited(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute).Truncate(time.Second)
	for _, c := range []struct {
		name    string
		headers http.Header
		want    time.Time
	}{
		{"x-ratelimit-reset", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)}}, reset},
		{"retry-after", http.Header{"Retry-After": {"120"}}, time.Now().Add(2 * time.Minute)},
		{"no reset", http.Header{"X-Ratelimit-Remaining": {"0"}}, time.Now().Add(time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := fakeRelease(t, "v0.5.0")
			f.limited = c.headers
			exe := install(t)
			_, err := latest(t).Apply(context.Background(), exe)
			var rl *RateLimitError
			if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) {
				t.Fatalf("Apply = %v, want a *RateLimitError", err)
			}
			if d := rl.Reset.Sub(c.want); d < -5*time.Second || d > 5*time.Second {
				t.Fatalf("reset %v, want about %v", rl.Reset, c.want)
			}
			assertUntouched(t, exe)
		})
	}
}

// A 403 that is not a rate limit is an ordinary error.
func TestApplyForbiddenIsNotRateLimit(t *testing.T) {
	f := fakeRelease(t, "v0.5.0")
	f.limited = http.Header{"X-Ratelimit-Remaining": {"42"}}
	exe := install(t)
	if _, err := latest(t).Apply(context.Background(), exe); err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("Apply = %v, want a plain HTTP 403 error", err)
	}
	assertUntouched(t, exe)
}
func TestLatestRejectsNonVersionTag(t *testing.T) {
	fakeRelease(t, "nightly")
	if rel, err := Latest(context.Background()); err == nil {
		t.Fatalf("accepted tag nightly: %+v", rel)
	}
}

func latest(t *testing.T) *Release {
	t.Helper()
	rel, err := Latest(context.Background())
	if err != nil || rel == nil {
		t.Fatalf("Latest = %v, %v", rel, err)
	}
	return rel
}

func TestApplyReplacesTheExecutable(t *testing.T) {
	fakeRelease(t, "v0.5.0")
	exe := install(t)
	paths, err := latest(t).Apply(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != exe {
		t.Fatalf("replaced %v, want %s", paths, exe)
	}
	if got := read(t, exe); got != string(newBin) {
		t.Errorf("executable = %q, want %q", got, newBin)
	}
	if _, err := os.Stat(newPath(exe)); !os.IsNotExist(err) {
		t.Error(".new left behind")
	}
	if err := Cleanup(exe); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Fatalf("folder after cleanup has %d entries, want 1", len(entries))
	}
}

func TestApplyRefusesUnknownExecutable(t *testing.T) {
	fakeRelease(t, "v0.5.0")
	for _, name := range []string{"other", "agentlink-tray"} {
		other := filepath.Join(t.TempDir(), fileName(name))
		if err := os.WriteFile(other, []byte("x"), 0o755); err != nil { // #nosec G306
			t.Fatal(err)
		}
		if _, err := latest(t).Apply(context.Background(), other); err == nil {
			t.Fatalf("Apply replaced %s, which is not agentlink", name)
		}
	}
}

// assertUntouched checks the old program is in place and nothing was parked.
func assertUntouched(t *testing.T, exe string) {
	t.Helper()
	if got := read(t, exe); got != "old" {
		t.Errorf("executable = %q after a refused update", got)
	}
	for _, leftover := range []string{OldPath(exe), newPath(exe)} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s left behind", filepath.Base(leftover))
		}
	}
}

func TestApplyRefusesBadDigest(t *testing.T) {
	fakeGitHub(t, "v0.5.0", []byte("EVIL"), digestOf(newBin))
	exe := install(t)
	_, err := latest(t).Apply(context.Background(), exe)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Apply = %v, want a checksum mismatch", err)
	}
	assertUntouched(t, exe)
}

func TestApplyRefusesMissingDigest(t *testing.T) {
	exe := install(t)
	for _, digest := range []string{
		"",                                   // no digest field
		"sha512:" + strings.Repeat("ab", 64), // another algorithm
		digestPrefix + "abc",                 // not a SHA-256
		digestPrefix + strings.Repeat("zz", 32),
	} {
		fakeGitHub(t, "v0.5.0", newBin, digest)
		if _, err := latest(t).Apply(context.Background(), exe); err == nil || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("digest %q: Apply = %v, want a missing-digest refusal", digest, err)
		}
		assertUntouched(t, exe)
	}
}

// A failure while swapping puts the old executable back.
func TestApplyRollsBack(t *testing.T) {
	fakeRelease(t, "v0.5.0")
	exe := install(t)
	rel := latest(t)
	for fail := 1; fail <= 2; fail++ {
		t.Run(fmt.Sprint("rename ", fail), func(t *testing.T) {
			n := 0
			rename = func(from, to string) error {
				n++
				if n == fail {
					return errors.New("injected")
				}
				return os.Rename(from, to)
			}
			defer func() { rename = os.Rename }()
			if _, err := rel.Apply(context.Background(), exe); err == nil {
				t.Fatal("Apply succeeded despite a failed rename")
			}
			assertUntouched(t, exe)
		})
	}
}

// Cleanup sweeps update leftovers and the executables of older releases.
func TestCleanupRemovesLegacy(t *testing.T) {
	exe := install(t)
	dir := filepath.Dir(exe)
	tray := filepath.Join(dir, fileName("agentlink-tray"))
	for _, p := range []string{tray, OldPath(tray), OldPath(exe), newPath(exe)} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Cleanup(exe); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(exe) {
		t.Fatalf("folder after cleanup: %v", entries)
	}
	if !LegacyName("AgentLink-Tray"+filepath.Ext(tray)) || LegacyName(filepath.Base(exe)) {
		t.Fatal("LegacyName")
	}
}

func TestParseDigest(t *testing.T) {
	h := strings.Repeat("ab", 32)
	if got := parseDigest("sha256:" + strings.ToUpper(h)); got != h {
		t.Errorf("parseDigest = %q", got)
	}
	for _, d := range []string{"", h, "sha256:", "md5:" + h, "sha256:" + h + "00"} {
		if got := parseDigest(d); got != "" {
			t.Errorf("parseDigest(%q) = %q, want empty", d, got)
		}
	}
}

func TestRequireHTTPS(t *testing.T) {
	if err := requireHTTPS("http://evil.example/agentlink.exe"); err == nil {
		t.Error("accepted a plaintext URL against the production apiBase")
	}
	if err := requireHTTPS("https://github.com/x/y/releases/download/v1/z"); err != nil {
		t.Errorf("rejected an https URL: %v", err)
	}
}

func TestOldPath(t *testing.T) {
	exe := filepath.Join("C:", "tools", "agentlink.exe")
	if got, want := OldPath(exe), filepath.Join("C:", "tools", ".agentlink.exe.old"); got != want {
		t.Fatalf("OldPath = %q, want %q", got, want)
	}
}
