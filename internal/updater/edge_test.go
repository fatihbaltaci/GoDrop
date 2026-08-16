package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- managed

func TestAManagedInstallationIsRefusedRatherThanOverwritten(t *testing.T) {
	// Writing over a file that dpkg, rpm or Homebrew believes it owns leaves a
	// machine whose package database and disk disagree, and the next upgrade
	// silently undoes the update.
	target := installed(t, "OLD BINARY")

	cases := []struct {
		name    string
		arrange func(t *testing.T) string // returns the binary path to try
		manager string
		hint    string
	}{
		{
			name: "container",
			arrange: func(t *testing.T) string {
				original := inContainer
				inContainer = func() bool { return true }
				t.Cleanup(func() { inContainer = original })
				return target
			},
			manager: "a container image",
			hint:    "docker pull",
		},
		{
			name: "dpkg",
			arrange: func(t *testing.T) string {
				fakeLookup(t, "dpkg")
				return target
			},
			manager: "dpkg",
			hint:    "apt",
		},
		{
			name: "rpm",
			arrange: func(t *testing.T) string {
				fakeLookup(t, "rpm")
				return target
			},
			manager: "rpm",
			hint:    "dnf",
		},
		{
			name: "homebrew",
			arrange: func(t *testing.T) string {
				fakeLookup(t, "nothing")
				return "/opt/homebrew/bin/godrop"
			},
			manager: "Homebrew",
			hint:    "brew upgrade",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skipIfManaged(t)
			path := tc.arrange(t)

			res, err := Update(context.Background(), "1.0.0", Options{BinaryPath: path})
			if !errors.Is(err, ErrManaged) {
				t.Fatalf("err = %v, want ErrManaged", err)
			}
			if res.ManagedBy != tc.manager || !strings.Contains(res.ManagedHint, tc.hint) {
				t.Errorf("result = %+v, want %q and a hint mentioning %q", res, tc.manager, tc.hint)
			}
			if data, _ := os.ReadFile(target); string(data) != "OLD BINARY" {
				t.Error("the installed binary was touched")
			}
		})
	}
}

// fakeLookup makes exactly one command name report that it owns the file.
func fakeLookup(t *testing.T, owner string) {
	t.Helper()
	original := lookup
	lookup = func(name string, _ ...string) error {
		if name == owner {
			return nil
		}
		return errors.New("not installed")
	}
	t.Cleanup(func() { lookup = original })
}

func TestHomebrewIsRecognisedWhereverItIsInstalled(t *testing.T) {
	fakeLookup(t, "nothing")
	for _, path := range []string{
		"/opt/homebrew/bin/godrop",
		"/usr/local/Cellar/godrop/1.0.0/bin/godrop",
		"/home/linuxbrew/.linuxbrew/bin/godrop",
	} {
		if !underHomebrew(path) {
			t.Errorf("underHomebrew(%q) = false", path)
		}
	}
	if underHomebrew("/usr/local/bin/godrop") {
		t.Error("an ordinary install is not Homebrew's")
	}
}

func TestAnOrdinaryInstallationIsNotManaged(t *testing.T) {
	skipIfManaged(t)
	fakeLookup(t, "nothing")
	if manager, _ := managedBy(installed(t, "x")); manager != "" {
		t.Errorf("managedBy = %q, want it left alone", manager)
	}
}

func TestContainerDetectionHasAWorkingDefault(t *testing.T) {
	// The value depends on where the suite runs; exercising it is the point.
	_ = inContainer()
	if lookup("definitely-not-a-command-4f2a") == nil {
		t.Error("looking up a command that does not exist should fail")
	}
	if lookup("go", "version") != nil {
		t.Error("looking up a command that does exist should succeed")
	}
}

// ------------------------------------------------------------ the last gate

func TestTheDownloadedBinaryIsRunBeforeItIsInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-godrop")
	script := "#!/bin/sh\necho \"godrop 9.9.9 (test)\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	out, err := execVersion(path)
	if err != nil || !strings.Contains(out, "godrop 9.9.9") {
		t.Fatalf("execVersion = %q, %v", out, err)
	}
	if _, err := execVersion(filepath.Join(dir, "not-there")); err == nil {
		t.Error("a binary that cannot be executed should be reported")
	}
	// The production path uses this, and a real update depends on it working.
	if _, err := runVersion(path); err != nil {
		t.Errorf("runVersion: %v", err)
	}
}

// --------------------------------------------------------------- replacing

func TestTheWindowsSwapRestoresTheOldBinaryIfItFails(t *testing.T) {
	// Windows cannot overwrite a running executable, so the old one is moved
	// aside first. If the second move fails, what was installed must come back.
	dir := t.TempDir()
	target := filepath.Join(dir, "godrop.exe")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	// Nothing staged, so the second move fails and the old binary must return.
	if err := replaceOn("windows", filepath.Join(dir, "vanished"), target); err == nil {
		t.Fatal("expected the swap to fail")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "OLD" {
		t.Errorf("the previous binary was not restored: %q (%v)", data, err)
	}
}

func TestTheWindowsSwapSaysWhereTheOldBinaryWentIfItCannotRestoreIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "godrop.exe")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	// Move it aside, then fail everything after that.
	calls := 0
	original := renameFile
	renameFile = func(from, to string) error {
		calls++
		if calls == 1 {
			return original(from, to)
		}
		return errors.New("disk gone")
	}
	t.Cleanup(func() { renameFile = original })

	err := replaceOn("windows", filepath.Join(dir, "staged"), target)
	if err == nil || !strings.Contains(err.Error(), target+".old") {
		t.Fatalf("err = %v, want it to say where the previous binary is", err)
	}
	if data, readErr := os.ReadFile(target + ".old"); readErr != nil || string(data) != "OLD" {
		t.Errorf("the previous binary should still be there: %q (%v)", data, readErr)
	}
}

func TestTheWindowsSwapReplacesTheBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "godrop.exe")
	staged := filepath.Join(dir, "staged.exe")
	for path, content := range map[string]string{target: "OLD", staged: "NEW"} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec
			t.Fatal(err)
		}
	}
	// A leftover backup from a previous update must not get in the way.
	if err := os.WriteFile(target+".old", []byte("ANCIENT"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	if err := replaceOn("windows", staged, target); err != nil {
		t.Fatalf("replaceOn: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "NEW" {
		t.Errorf("installed content = %q", data)
	}
}

func TestTheWindowsSwapReportsAnUnmovableBinary(t *testing.T) {
	dir := t.TempDir()
	// Nothing to move aside: the target does not exist.
	if err := replaceOn("windows", filepath.Join(dir, "staged"), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected an error")
	}
}

// --------------------------------------------------------------- the rest

func TestLatestReportsWhatWentWrong(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		url     string
		wantErr string
	}{
		{
			name:    "no releases published yet",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantErr: "no releases yet",
		},
		{
			name:    "github is unwell",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
			wantErr: "502",
		},
		{
			name:    "unreadable body",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{oops")) },
			wantErr: "could not read the latest release",
		},
		{
			name:    "no releases yet",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
			wantErr: "could not read the latest release",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if _, err := Latest(context.Background(), Options{APIURL: srv.URL, HTTP: srv.Client()}); err == nil ||
				!strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	if _, err := Latest(context.Background(), Options{APIURL: "http://\x7f"}); err == nil {
		t.Error("an unusable API URL should be reported")
	}
	if _, err := Latest(context.Background(), Options{APIURL: "http://127.0.0.1:1"}); err == nil {
		t.Error("an unreachable API should be reported")
	}
}

func TestUpdateReportsAFailedVersionLookup(t *testing.T) {
	skipIfManaged(t)
	fakeLookup(t, "nothing")
	_, err := Update(context.Background(), "1.0.0", Options{
		BinaryPath: installed(t, "OLD"),
		APIURL:     "http://127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("an unreachable github should stop the update")
	}
}

func TestUpdateCannotStageNextToAnUnwritableBinary(t *testing.T) {
	requireStrictPermissions(t)
	skipIfManaged(t)
	fakeLookup(t, "nothing")

	dir := t.TempDir()
	target := filepath.Join(dir, "godrop")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	rel := newRelease(t, "v2.0.0", "NEW")
	opts := options(t, rel.serve(t), target)
	// The message names the directory and the command that gets past it,
	// because "permission denied" on a path nobody typed is not an answer.
	_, err := Update(context.Background(), "1.0.0", opts)
	if err == nil || !strings.Contains(err.Error(), "not writable") ||
		!strings.Contains(err.Error(), "sudo godrop update") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "OLD" {
		t.Error("the installed binary was disturbed")
	}
}

func TestStagingReportsADirectoryItCannotWriteTo(t *testing.T) {
	requireStrictPermissions(t)
	// Update checks the directory before downloading, so this is the race
	// where it stops being writable in between. The guard stays because the
	// alternative is a partial file in somebody's bin directory.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	rel := newRelease(t, "v2.0.0", "NEW")
	opts := options(t, rel.serve(t), filepath.Join(dir, "godrop"))
	if _, _, err := download(context.Background(), opts, "v2.0.0", dir); err == nil ||
		!strings.Contains(err.Error(), "cannot write next to the current binary") {
		t.Fatalf("err = %v", err)
	}
}

func TestAFailedStagingWriteLeavesNothingBehind(t *testing.T) {
	skipIfManaged(t)
	fakeLookup(t, "nothing")
	original := writeStaged
	writeStaged = func(*os.File, []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { writeStaged = original })

	target := installed(t, "OLD")
	rel := newRelease(t, "v2.0.0", "NEW")
	if _, err := Update(context.Background(), "1.0.0", options(t, rel.serve(t), target)); err == nil ||
		!strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "OLD" {
		t.Error("the installed binary was disturbed")
	}
	if left := staged(t, filepath.Dir(target)); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestFetchReportsAnUnusableURL(t *testing.T) {
	if _, err := fetch(context.Background(), Options{}, "http://\x7f"); err == nil {
		t.Error("an unusable URL should be reported")
	}
	if _, err := fetch(context.Background(), Options{}, "http://127.0.0.1:1/x"); err == nil {
		t.Error("an unreachable host should be reported")
	}
}

func TestAZipEntryThatCannotBeOpenedIsReported(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// An unknown compression method: the entry is listed but cannot be read.
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "godrop.exe", Method: 99, CompressedSize64: 4, UncompressedSize64: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fromZip(buf.Bytes()); err == nil {
		t.Fatal("an entry that cannot be opened should be reported")
	}
}

func TestExtractRejectsRubbish(t *testing.T) {
	if _, err := fromZip([]byte("not a zip")); err == nil || !strings.Contains(err.Error(), "not a zip") {
		t.Errorf("err = %v", err)
	}
	if _, err := extract([]byte("not a tarball"), "linux"); err == nil {
		t.Error("a broken tarball should be reported")
	}
}

func TestDefaultsAreUsable(t *testing.T) {
	if client(Options{}) != http.DefaultClient {
		t.Error("the default HTTP client should be used when none is given")
	}
	// Without an explicit path, the running test binary is what would be
	// replaced, which is exactly the behaviour a real update relies on.
	path, err := binaryPath(Options{})
	if err != nil || path == "" {
		t.Fatalf("binaryPath = %q, %v", path, err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("binaryPath = %q, want an absolute path", path)
	}
}

func TestSameVersionIgnoresTheLeadingV(t *testing.T) {
	if !SameVersion("v1.2.3", "1.2.3") || SameVersion("1.2.3", "1.2.4") {
		t.Error("versions should compare without their leading v")
	}
}

// requireStrictPermissions skips a test that depends on a directory being
// unwritable, which Windows does not model and root ignores.
func requireStrictPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}
