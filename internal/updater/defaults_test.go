package updater

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubTransport answers every request from a table, so the production URLs,
// github.com and api.github.com, can be exercised without a network.
type stubTransport struct {
	responses map[string]func() (*http.Response, error)
	seen      []string
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.seen = append(s.seen, req.URL.String())
	for suffix, respond := range s.responses {
		if strings.HasSuffix(req.URL.Path, suffix) {
			return respond()
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("not found")),
		Header:     http.Header{},
	}, nil
}

func ok(body []byte) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{},
		}, nil
	}
}

// TestTheProductionURLsAreTheOnesUsed exercises every default: the GitHub API,
// the release download host, and the running platform's archive name.
func TestTheProductionURLsAreTheOnesUsed(t *testing.T) {
	skipIfManaged(t)
	fakeLookup(t, "nothing")

	payload := "NEW BINARY"
	archive := tarGz(t, "godrop", payload)
	name := fmt.Sprintf("godrop_2.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		archive = zipped(t, "godrop.exe", payload)
		name = fmt.Sprintf("godrop_2.0.0_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	}
	sum := sha256.Sum256(archive)

	stub := &stubTransport{responses: map[string]func() (*http.Response, error){
		"/releases/latest": ok([]byte(`{"tag_name":"v2.0.0"}`)),
		"/SHA256SUMS":      ok([]byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")),
		"/" + name:         ok(archive),
	}}

	target := installed(t, "OLD BINARY")
	res, err := Update(context.Background(), "1.0.0", Options{
		BinaryPath: target,
		HTTP:       &http.Client{Transport: stub},
		Verify:     func(string) (string, error) { return "godrop 2.0.0", nil },
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.Updated {
		t.Fatalf("result = %+v", res)
	}
	if data, _ := os.ReadFile(target); string(data) != payload {
		t.Errorf("installed content = %q", data)
	}

	joined := strings.Join(stub.seen, " ")
	for _, want := range []string{
		"https://api.github.com/repos/" + Repo + "/releases/latest",
		"https://github.com/" + Repo + "/releases/download/v2.0.0/" + name,
		"https://github.com/" + Repo + "/releases/download/v2.0.0/SHA256SUMS",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("never requested %s (asked for %v)", want, stub.seen)
		}
	}
}

func TestAMissingChecksumFileStopsTheUpdate(t *testing.T) {
	skipIfManaged(t)
	fakeLookup(t, "nothing")
	rel := newRelease(t, "v2.0.0", "NEW")
	srv := rel.serve(t)
	rel.sums = "" // served as an empty 200, so ask for a 404 instead

	stub := &stubTransport{responses: map[string]func() (*http.Response, error){
		"/releases/latest":                 ok([]byte(`{"tag_name":"v2.0.0"}`)),
		"/godrop_2.0.0_linux_amd64.tar.gz": ok(rel.files["godrop_2.0.0_linux_amd64.tar.gz"]),
	}}
	target := installed(t, "OLD")
	opts := options(t, srv, target)
	opts.HTTP = &http.Client{Transport: stub}

	_, err := Update(context.Background(), "1.0.0", opts)
	if err == nil || !strings.Contains(err.Error(), "refusing to install an unverified binary") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "OLD" {
		t.Error("the installed binary was disturbed")
	}
}

// TestTheDefaultVerifierRunsTheDownload proves the last gate is wired up: with
// no Verify supplied, the downloaded file is executed before it is installed.
func TestTheDefaultVerifierRunsTheDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the release stand-in is a POSIX shell script")
	}
	skipIfManaged(t)
	fakeLookup(t, "nothing")

	script := "#!/bin/sh\necho \"godrop 2.0.0 (test)\"\n"
	rel := newRelease(t, "v2.0.0", script)
	target := installed(t, "OLD")
	opts := options(t, rel.serve(t), target)
	opts.Verify = nil // use the real one

	if _, err := Update(context.Background(), "1.0.0", opts); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != script {
		t.Errorf("installed content = %q", data)
	}
}

func TestUpdateReportsAFailedSwap(t *testing.T) {
	skipIfManaged(t)
	fakeLookup(t, "nothing")
	original := renameFile
	renameFile = func(string, string) error { return errors.New("cross-device link") }
	t.Cleanup(func() { renameFile = original })

	target := installed(t, "OLD")
	rel := newRelease(t, "v2.0.0", "NEW")
	if _, err := Update(context.Background(), "1.0.0", options(t, rel.serve(t), target)); err == nil ||
		!strings.Contains(err.Error(), "cross-device link") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "OLD" {
		t.Error("the installed binary was disturbed")
	}
}

func TestUpdateReportsABinaryItCannotLocate(t *testing.T) {
	original := osExecutable
	osExecutable = func() (string, error) { return "", errors.New("no /proc") }
	t.Cleanup(func() { osExecutable = original })

	if _, err := Update(context.Background(), "1.0.0", Options{}); err == nil {
		t.Fatal("an unknown binary path should stop the update")
	}
}

func TestBinaryPathFallsBackToTheUnresolvedPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	original := osExecutable
	osExecutable = func() (string, error) { return missing, nil }
	t.Cleanup(func() { osExecutable = original })

	// EvalSymlinks cannot resolve a path that does not exist; the update still
	// knows which file it means.
	if got, err := binaryPath(Options{}); err != nil || got != missing {
		t.Errorf("binaryPath = %q, %v, want %q", got, err, missing)
	}
}

func TestLatestReportsAnUnreadableBody(t *testing.T) {
	stub := &stubTransport{responses: map[string]func() (*http.Response, error){
		"/releases/latest": func() (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(failingReader{}),
				Header:     http.Header{},
			}, nil
		},
	}}
	if _, err := Latest(context.Background(), Options{HTTP: &http.Client{Transport: stub}}); err == nil {
		t.Fatal("a body that cannot be read should be reported")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestATarballThatEndsHalfwayIsReported(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("this is not a tar stream at all, just bytes")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fromTarGz(buf.Bytes()); err == nil {
		t.Fatal("a broken tar stream should be reported")
	}
}

func TestContainerMarkersAreBothChecked(t *testing.T) {
	dir := t.TempDir()
	dockerEnv := filepath.Join(dir, ".dockerenv")
	cgroup := filepath.Join(dir, "cgroup")

	if inContainerAt(dockerEnv, cgroup) {
		t.Error("neither marker is present")
	}
	if err := os.WriteFile(cgroup, []byte("0::/system.slice/docker-abc.scope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !inContainerAt(dockerEnv, cgroup) {
		t.Error("a docker cgroup should be recognised")
	}
	if err := os.WriteFile(cgroup, []byte("0::/user.slice"), 0o600); err != nil {
		t.Fatal(err)
	}
	if inContainerAt(dockerEnv, cgroup) {
		t.Error("an ordinary cgroup is not a container")
	}
	if err := os.WriteFile(dockerEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !inContainerAt(dockerEnv, cgroup) {
		t.Error("/.dockerenv should be enough on its own")
	}
}
