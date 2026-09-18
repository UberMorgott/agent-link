package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func asset(p string) string { return AssetName(p, runtime.GOOS, runtime.GOARCH) }

// fakeGitHub serves one release with tag: bins maps a program to its
// executable (nil: no asset), sums is checksums.txt (nil: no such asset).
// apiBase points at it for the test.
func fakeGitHub(t *testing.T, tag string, bins map[string][]byte, sums []byte) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+Repo+"/releases/latest":
			var list []string
			for p := range bins {
				list = append(list, fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/dl/%s"}`, asset(p), srv.URL, p))
			}
			if sums != nil {
				list = append(list, fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/sums"}`, ChecksumsAsset, srv.URL))
			}
			_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[%s]}`, tag, strings.Join(list, ","))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			_, _ = w.Write(bins[strings.TrimPrefix(r.URL.Path, "/dl/")])
		case r.URL.Path == "/sums":
			_, _ = w.Write(sums)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	saved := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = saved })
}

// sumsOf lists bins in `sha256sum` format, after an unrelated line.
func sumsOf(bins map[string][]byte) []byte {
	s := strings.Repeat("0", 64) + "  someone-else\n"
	for p, b := range bins {
		h := sha256.Sum256(b)
		s += hex.EncodeToString(h[:]) + "  " + asset(p) + "\n"
	}
	return []byte(s)
}

func newBins() map[string][]byte {
	return map[string][]byte{"agentlink": []byte("new cli"), "agentlink-tray": []byte("new tray")}
}

// install writes the old programs into a folder and returns the tray's path.
func install(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range Programs {
		if err := os.WriteFile(filepath.Join(dir, fileName(p)), []byte("old "+p), 0o755); err != nil { // #nosec G306
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, fileName("agentlink-tray"))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- a test temp file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sibling(exe, p string) string { return filepath.Join(filepath.Dir(exe), fileName(p)) }

func TestAssetNames(t *testing.T) {
	cases := map[[3]string]string{
		{"agentlink", "windows", "amd64"}:      "agentlink.exe",
		{"agentlink-tray", "windows", "amd64"}: "agentlink-tray.exe",
		{"agentlink", "linux", "amd64"}:        "agentlink-linux-amd64",
		{"agentlink-tray", "windows", "arm64"}: "agentlink-tray-windows-arm64.exe",
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
	bins := newBins()
	for _, c := range []struct {
		name, tag, cur string
		newer          bool
	}{
		{"newer", "v0.5.0", "0.4.0", true},
		{"older", "v0.3.0", "0.4.0", false},
		{"equal", "v0.4.0", "0.4.0", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeGitHub(t, c.tag, bins, sumsOf(bins))
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

// An older release without checksums (v0.3.0 was published that way) is not
// an error; a newer one is: it could never be applied.
func TestCheckMissingChecksums(t *testing.T) {
	fakeGitHub(t, "v0.3.0", newBins(), nil)
	if _, newer, err := Check(context.Background(), "0.4.0"); err != nil || newer {
		t.Fatalf("older release without checksums: newer=%v err=%v", newer, err)
	}
	fakeGitHub(t, "v0.5.0", newBins(), nil)
	if _, newer, err := Check(context.Background(), "0.4.0"); err == nil || newer || !strings.Contains(err.Error(), ChecksumsAsset) {
		t.Fatalf("newer release without checksums: newer=%v err=%v", newer, err)
	}
}

func TestLatestNothingToUpdateTo(t *testing.T) {
	fakeGitHub(t, "v0.5.0", nil, []byte("x"))
	if rel, err := Latest(context.Background()); rel != nil || err != nil {
		t.Fatalf("no asset for this platform: %v, %v", rel, err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	apiBase = srv.URL
	if rel, err := Latest(context.Background()); rel != nil || err != nil {
		t.Fatalf("no release: %v, %v", rel, err)
	}
}

func TestLatestRejectsNonVersionTag(t *testing.T) {
	fakeGitHub(t, "nightly", newBins(), sumsOf(newBins()))
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

func TestApplyReplacesBothPrograms(t *testing.T) {
	bins := newBins()
	fakeGitHub(t, "v0.5.0", bins, sumsOf(bins))
	exe := install(t)
	paths, err := latest(t).Apply(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("replaced %v, want both programs", paths)
	}
	for _, p := range Programs {
		path := sibling(exe, p)
		if got := read(t, path); got != string(bins[p]) {
			t.Errorf("%s = %q, want %q", p, got, bins[p])
		}
		if _, err := os.Stat(newPath(path)); !os.IsNotExist(err) {
			t.Errorf("%s: .new left behind", p)
		}
	}
	if err := Cleanup(exe); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 2 {
		t.Fatalf("folder after cleanup has %d entries, want 2", len(entries))
	}
}

// Without the CLI next to it, only the running program is replaced.
func TestApplyOnlyExistingSiblings(t *testing.T) {
	bins := newBins()
	fakeGitHub(t, "v0.5.0", bins, sumsOf(bins))
	exe := install(t)
	if err := os.Remove(sibling(exe, "agentlink")); err != nil {
		t.Fatal(err)
	}
	paths, err := latest(t).Apply(context.Background(), exe)
	if err != nil || len(paths) != 1 || paths[0] != exe {
		t.Fatalf("Apply = %v, %v", paths, err)
	}
	if _, err := os.Stat(sibling(exe, "agentlink")); !os.IsNotExist(err) {
		t.Fatal("Apply created the missing CLI")
	}
}

func TestApplyRefusesUnknownExecutable(t *testing.T) {
	bins := newBins()
	fakeGitHub(t, "v0.5.0", bins, sumsOf(bins))
	other := filepath.Join(t.TempDir(), "other.exe")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil { // #nosec G306
		t.Fatal(err)
	}
	if _, err := latest(t).Apply(context.Background(), other); err == nil {
		t.Fatal("Apply replaced a program that is not agentlink")
	}
}

// assertUntouched checks the old programs are in place and nothing was parked.
func assertUntouched(t *testing.T, exe string) {
	t.Helper()
	for _, p := range Programs {
		path := sibling(exe, p)
		if got := read(t, path); got != "old "+p {
			t.Errorf("%s = %q after a refused update", p, got)
		}
		for _, leftover := range []string{OldPath(path), newPath(path)} {
			if _, err := os.Stat(leftover); !os.IsNotExist(err) {
				t.Errorf("%s left behind", filepath.Base(leftover))
			}
		}
	}
}

func TestApplyRefusesBadChecksum(t *testing.T) {
	bins := newBins()
	honest := sumsOf(bins)
	bins["agentlink"] = []byte("EVIL")
	fakeGitHub(t, "v0.5.0", bins, honest)
	exe := install(t)
	_, err := latest(t).Apply(context.Background(), exe)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Apply = %v, want a checksum mismatch", err)
	}
	assertUntouched(t, exe)
}

func TestApplyRefusesMissingChecksums(t *testing.T) {
	bins := newBins()
	fakeGitHub(t, "v0.5.0", bins, nil)
	exe := install(t)
	if _, err := latest(t).Apply(context.Background(), exe); err == nil {
		t.Fatal("Apply installed a release without checksums.txt")
	}
	// A checksums.txt that does not list one of the programs is refused too.
	one := map[string][]byte{"agentlink-tray": bins["agentlink-tray"]}
	fakeGitHub(t, "v0.5.0", bins, sumsOf(one))
	if _, err := latest(t).Apply(context.Background(), exe); err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("Apply = %v, want an unlisted-asset refusal", err)
	}
	assertUntouched(t, exe)
}

// A failure while swapping the second program puts the first one back.
func TestApplyRollsBack(t *testing.T) {
	bins := newBins()
	fakeGitHub(t, "v0.5.0", bins, sumsOf(bins))
	exe := install(t)
	rel := latest(t)
	for fail := 1; fail <= 4; fail++ {
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

func TestSumFor(t *testing.T) {
	h := strings.Repeat("ab", 32)
	for _, sums := range []string{
		"deadbeef  other\n" + h + "  agentlink.exe\n",
		h + " *agentlink.exe\n",
		h + "  agentlink.exe",
	} {
		if got, err := sumFor([]byte(sums), "agentlink.exe"); err != nil || got != h {
			t.Errorf("sumFor(%q) = %q, %v", sums, got, err)
		}
	}
	if _, err := sumFor([]byte("abc  agentlink.exe\n"), "agentlink.exe"); err == nil {
		t.Error("sumFor accepted a hash that is not SHA-256")
	}
	if _, err := sumFor([]byte(h+"  agentlink-tray.exe\n"), "agentlink.exe"); err == nil {
		t.Error("sumFor accepted an unlisted asset")
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
	exe := filepath.Join("C:", "tools", "agentlink-tray.exe")
	if got, want := OldPath(exe), filepath.Join("C:", "tools", ".agentlink-tray.exe.old"); got != want {
		t.Fatalf("OldPath = %q, want %q", got, want)
	}
}
