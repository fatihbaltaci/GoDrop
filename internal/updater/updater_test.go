package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	"strconv"
	"strings"
	"testing"
	"time"
)

// release builds the artefacts a real release publishes: an archive holding a
// binary, and a SHA256SUMS listing its digest.
type release struct {
	version  string
	files    map[string][]byte // name -> archive bytes
	sums     string
	requests []string
}

func newRelease(t *testing.T, version, payload string) *release {
	t.Helper()
	r := &release{version: version, files: map[string][]byte{}}
	for _, target := range []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"},
	} {
		ext, archive := "tar.gz", tarGz(t, "godrop", payload)
		if target.goos == "windows" {
			ext, archive = "zip", zipped(t, "godrop.exe", payload)
		}
		name := fmt.Sprintf("godrop_%s_%s_%s.%s",
			strings.TrimPrefix(version, "v"), target.goos, target.goarch, ext)
		r.files[name] = archive
		sum := sha256.Sum256(archive)
		r.sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	return r
}

// serve stands in for GitHub: the API and the release downloads.
func (r *release) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests = append(r.requests, req.URL.Path)
		switch {
		case strings.HasSuffix(req.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":%q}`, r.version)
		case strings.HasSuffix(req.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write([]byte(r.sums))
		default:
			name := filepath.Base(req.URL.Path)
			body, ok := r.files[name]
			if !ok {
				http.NotFound(w, req)
				return
			}
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name, body string
	}{{"README.md", "docs"}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipped(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installed writes a stand-in for the currently installed binary.
func installed(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "godrop")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	return path
}

func options(t *testing.T, srv *httptest.Server, target string) Options {
	t.Helper()
	return Options{
		BinaryPath: target,
		BaseURL:    srv.URL + "/releases/download",
		APIURL:     srv.URL,
		HTTP:       srv.Client(),
		GOOS:       "linux",
		GOARCH:     "amd64",
		Verify:     func(string) (string, error) { return "godrop 2.0.0 (abc, built today)", nil },
	}
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	skipIfManaged(t)
	rel := newRelease(t, "v2.0.0", "NEW BINARY")
	srv := rel.serve(t)
	target := installed(t, "OLD BINARY")

	res, err := Update(context.Background(), "1.0.0", options(t, srv, target))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.Updated || res.To != "v2.0.0" {
		t.Errorf("result = %+v", res)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "NEW BINARY" {
		t.Errorf("installed content = %q", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want it executable", info.Mode())
	}
	if left := staged(t, filepath.Dir(target)); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestUpdateDoesNothingWhenAlreadyCurrent(t *testing.T) {
	skipIfManaged(t)
	rel := newRelease(t, "v2.0.0", "NEW BINARY")
	srv := rel.serve(t)
	target := installed(t, "OLD BINARY")

	res, err := Update(context.Background(), "v2.0.0", options(t, srv, target))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.UpToDate || res.Updated {
		t.Errorf("result = %+v", res)
	}
	if data, _ := os.ReadFile(target); string(data) != "OLD BINARY" {
		t.Error("nothing should have been downloaded or written")
	}
}

// The point of the package: every failure leaves the installation untouched.
func TestAFailedUpdateLeavesTheInstallationAlone(t *testing.T) {
	skipIfManaged(t)
	cases := []struct {
		name    string
		mutate  func(*release, *Options)
		wantErr string
	}{
		{
			name:    "checksum mismatch",
			mutate:  func(r *release, _ *Options) { r.wrongSums() },
			wantErr: "checksum mismatch",
		},
		{
			name:    "no checksum published",
			mutate:  func(r *release, _ *Options) { r.sums = "" },
			wantErr: "not listed in SHA256SUMS",
		},
		{
			name:    "archive missing",
			mutate:  func(r *release, _ *Options) { r.files = map[string][]byte{} },
			wantErr: "404",
		},
		{
			name: "downloaded binary does not run",
			mutate: func(_ *release, o *Options) {
				o.Verify = func(string) (string, error) { return "", errors.New("exec format error") }
			},
			wantErr: "does not run",
		},
		{
			name:    "downloaded binary is something else",
			mutate:  func(_ *release, o *Options) { o.Verify = func(string) (string, error) { return "curl 8.4.0", nil } },
			wantErr: "does not look like godrop",
		},
		{
			name:    "archive is not an archive",
			mutate:  func(r *release, _ *Options) { r.corrupt() },
			wantErr: "not gzip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := newRelease(t, "v2.0.0", "NEW BINARY")
			target := installed(t, "OLD BINARY")
			opts := options(t, rel.serve(t), target)
			tc.mutate(rel, &opts)

			_, err := Update(context.Background(), "1.0.0", opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			if data, readErr := os.ReadFile(target); readErr != nil || string(data) != "OLD BINARY" {
				t.Errorf("the installed binary was disturbed: %q (%v)", data, readErr)
			}
			if left := staged(t, filepath.Dir(target)); len(left) > 0 {
				t.Errorf("temporary files left behind: %v", left)
			}
		})
	}
}

// wrongSums keeps every file name but publishes the wrong digest for it.
func (r *release) wrongSums() {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(r.sums), "\n") {
		digest, name, _ := strings.Cut(line, "  ")
		out = append(out, strings.Repeat("0", len(digest))+"  "+name)
	}
	r.sums = strings.Join(out, "\n") + "\n"
}

// corrupt replaces the archives with bytes that are not an archive, keeping
// the checksums correct so the failure happens at unpacking.
func (r *release) corrupt() {
	r.sums = ""
	for name := range r.files {
		body := []byte("not an archive")
		r.files[name] = body
		sum := sha256.Sum256(body)
		r.sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
}

func TestUpdateFindsTheWindowsArchive(t *testing.T) {
	skipIfManaged(t)
	rel := newRelease(t, "v2.0.0", "NEW EXE")
	srv := rel.serve(t)
	target := installed(t, "OLD")

	opts := options(t, srv, target)
	opts.GOOS, opts.GOARCH = "windows", "amd64"
	if _, err := Update(context.Background(), "1.0.0", opts); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "NEW EXE" {
		t.Errorf("installed content = %q", data)
	}
}

func TestUpdateRejectsAZipWithoutTheBinary(t *testing.T) {
	skipIfManaged(t)
	rel := newRelease(t, "v2.0.0", "NEW EXE")
	for name := range rel.files {
		if strings.HasSuffix(name, ".zip") {
			body := zipped(t, "README.md", "docs")
			rel.files[name] = body
			sum := sha256.Sum256(body)
			rel.sums = strings.Join(replaceSum(rel.sums, name, hex.EncodeToString(sum[:])), "\n")
		}
	}
	opts := options(t, rel.serve(t), installed(t, "OLD"))
	opts.GOOS, opts.GOARCH = "windows", "amd64"
	if _, err := Update(context.Background(), "1.0.0", opts); err == nil ||
		!strings.Contains(err.Error(), "no godrop.exe") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateRejectsATarballWithoutTheBinary(t *testing.T) {
	skipIfManaged(t)
	rel := newRelease(t, "v2.0.0", "NEW")
	for name := range rel.files {
		if strings.HasSuffix(name, ".tar.gz") {
			body := tarGz(t, "LICENSE", "MIT")
			rel.files[name] = body
			sum := sha256.Sum256(body)
			rel.sums = strings.Join(replaceSum(rel.sums, name, hex.EncodeToString(sum[:])), "\n")
		}
	}
	if _, err := Update(context.Background(), "1.0.0", options(t, rel.serve(t), installed(t, "OLD"))); err == nil ||
		!strings.Contains(err.Error(), "no godrop binary") {
		t.Fatalf("err = %v", err)
	}
}

func replaceSum(sums, name, digest string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(sums), "\n") {
		if strings.HasSuffix(line, "  "+name) {
			out = append(out, digest+"  "+name)
			continue
		}
		out = append(out, line)
	}
	return out
}

// staged lists leftover temporary files.
func staged(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".godrop-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// skipIfManaged keeps the suite honest on a machine where the temporary
// directory happens to look managed, which it never should.
func skipIfManaged(t *testing.T) {
	t.Helper()
	original := inContainer
	inContainer = func() bool { return false }
	t.Cleanup(func() { inContainer = original })
}

func TestARateLimitSaysHowLongToWait(t *testing.T) {
	// A shared address can use up GitHub's anonymous allowance without the
	// person at the keyboard doing anything, and "403 Forbidden" reads like a
	// permission problem rather than a wait.
	at := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(at.Add(12*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := Latest(context.Background(), Options{
		APIURL: srv.URL,
		Now:    func() time.Time { return at },
	})
	if err == nil {
		t.Fatal("a rate limit is still a failure")
	}
	for _, want := range []string{"rate limit", "try again in 12m", "releases/latest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestTheClockDefaultsToTheRealOne(t *testing.T) {
	if got := now(Options{}); time.Since(got) > time.Minute {
		t.Errorf("now() = %v, want the current time", got)
	}
}

func TestResetIn(t *testing.T) {
	at := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"":             "",
		"not a number": "",
		strconv.FormatInt(at.Add(-time.Hour).Unix(), 10):     "",
		strconv.FormatInt(at.Add(90*time.Second).Unix(), 10): " (try again in 2m)",
		strconv.FormatInt(at.Add(time.Hour).Unix(), 10):      " (try again in 1h)",
		strconv.FormatInt(at.Add(90*time.Minute).Unix(), 10): " (try again in 1h30m)",
	}
	for header, want := range cases {
		if got := resetIn(header, at); got != want {
			t.Errorf("resetIn(%q) = %q, want %q", header, got, want)
		}
	}
}
