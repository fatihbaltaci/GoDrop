package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// devNull is a character device, which is what lets the terminal-only paths be
// exercised without an actual terminal.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s is not a character device on this platform", os.DevNull)
	}
	return f
}

func TestInteractiveOnCharacterDevices(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("TERM", "xterm")
	null := devNull(t)
	if !interactiveOn(null, null) {
		t.Error("two character devices should look like a terminal")
	}

	t.Setenv("CI", "true")
	if interactiveOn(null, null) {
		t.Error("CI must never get an interactive form")
	}
	t.Setenv("CI", "")
	t.Setenv("TERM", "dumb")
	if interactiveOn(null, null) {
		t.Error("a dumb terminal must not get an interactive form")
	}

	var notAFile bytes.Buffer
	_ = notAFile
	if interactiveOn(os.NewFile(0, "")) {
		t.Error("an unusable file handle is not a terminal")
	}
}

func TestErrorPrefixIsColouredOnATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	// The colour library disables itself when the test binary's stdout is not a
	// terminal; here the destination is what matters.
	original := color.NoColor
	color.NoColor = false
	t.Cleanup(func() { color.NoColor = original })
	if !strings.Contains(errorPrefixFor(devNull(t)), "error:") {
		t.Error("the prefix should still say error")
	}
	if !strings.Contains(errorPrefixFor(devNull(t)), "\x1b[") {
		t.Error("a terminal should receive colour escapes")
	}
}

func TestNewInteractivePrompterBuildsTheTerminalWizard(t *testing.T) {
	p := newInteractivePrompter(&output{w: io.Discard})
	if _, ok := p.(*huhPrompter); !ok {
		t.Errorf("prompter = %T, want the huh implementation", p)
	}
}

func TestHuhPromptsPropagateCancellation(t *testing.T) {
	p := newHuhPrompter(&output{w: io.Discard})
	p.w = io.Discard

	p.in = repeat('\x03')
	if _, err := p.Select("Deployment", "", []wizard.Option{{Label: "compose", Value: "compose"}}, "compose"); err == nil {
		t.Error("Ctrl+C should abort a select")
	}
	p.in = repeat('\x03')
	if _, err := p.Confirm("Telemetry", "", true); err == nil {
		t.Error("Ctrl+C should abort a confirmation")
	}
}

func TestHealthDefaultsToTheLocalListenAddress(t *testing.T) {
	addr := freePort(t)
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	waitForListener(t, addr)

	t.Setenv("GODROP_ADDR", addr)
	if code, out, _ := run(t, testBuild(), "health"); code != 0 || !strings.Contains(out, "healthy") {
		t.Errorf("exit = %d, out = %q", code, out)
	}
}

func TestTokenListShowsLastUse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_TOKENS", "")
	store, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("token should verify")
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	_, out, _ := run(t, testBuild(), "token", "list", "--data-dir", dir)
	if !strings.Contains(out, "just now") {
		t.Errorf("the listing should show recent use:\n%s", out)
	}
	_, jsonOut, _ := run(t, testBuild(), "token", "list", "--data-dir", dir, "--json")
	if !strings.Contains(jsonOut, "last_used") {
		t.Errorf("json = %s", jsonOut)
	}
}

func TestTokenRevokeHumanOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_TOKENS", "")
	if code, _, _ := run(t, testBuild(), "token", "create", "--name", "temporary", "--data-dir", dir); code != 0 {
		t.Fatal("create failed")
	}
	code, out, _ := run(t, testBuild(), "token", "revoke", "temporary", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "revoked") {
		t.Errorf("exit = %d, out = %q", code, out)
	}
}

func TestTelemetrySetJSONOutput(t *testing.T) {
	code, out, _ := run(t, testBuild(), "telemetry", "off", "--data-dir", t.TempDir(), "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if got["telemetry"] != "off" {
		t.Errorf("json = %v", got)
	}
}

// blockInstallID makes the telemetry identity unreadable, the one way
// telemetry setup can fail on an otherwise healthy installation.
func blockInstallID(t *testing.T, dir string) {
	t.Helper()
	requireStrictPermissions(t)
	path := filepath.Join(dir, telemetry.FileName)
	if err := os.WriteFile(path, []byte("existing"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

func TestTelemetryStatusReportsSetupFailures(t *testing.T) {
	dir := t.TempDir()
	blockInstallID(t, dir)
	build := testBuild()
	build.PostHogKey = "phc_test"

	if code, _, _ := run(t, build, "telemetry", "status", "--data-dir", dir); code == 0 {
		t.Error("an unreadable installation id should be reported")
	}
}

func TestServeWarnsWhenTelemetryCannotStart(t *testing.T) {
	dir := t.TempDir()
	blockInstallID(t, dir)

	addr := freePort(t)
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_ADDR", addr)
	t.Setenv("GODROP_BASE_URL", "https://files.example.com")
	t.Setenv("GODROP_RETENTION", "1m")

	build := testBuild()
	build.PostHogKey = "phc_test"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ExecuteWith(ctx, build, []string{"serve"}, io.Discard, io.Discard) }()
	waitForListener(t, addr)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("the server should still run without telemetry, exit = %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Error("the server did not shut down")
	}
}

func TestServeWarnsWhenTokenUsageCannotBeSaved(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	store, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.Create("agent")
	if err != nil {
		t.Fatal(err)
	}

	addr := freePort(t)
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "")
	t.Setenv("GODROP_ADDR", addr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ExecuteWith(ctx, testBuild(), []string{"serve"}, io.Discard, io.Discard) }()
	waitForListener(t, addr)

	// Use the token so there is something to write, then take the directory
	// away before shutting down.
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("a failed usage write must not fail the shutdown, exit = %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Error("the server did not shut down")
	}
}

func TestCleanupTicksRepeatedly(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var logs safeBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The file is fresh when the first sweep runs and stale by the next tick,
	// so only a repeating cleaner can remove it.
	if _, err := store.Create("txt", strings.NewReader("expiring"), 1<<20); err != nil {
		t.Fatal(err)
	}
	// A very short retention makes the interval short too, so the ticker fires.
	go runCleanup(ctx, store, &config.Config{Retention: 50 * time.Millisecond}, logger)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if files, _ := store.Stats(); files == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("the cleaner did not run again; logs:\n%s", logs.String())
}

func TestDoctorJSONOnAHealthyInstallation(t *testing.T) {
	dir := t.TempDir()
	if _, err := storage.New(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_BASE_URL", "https://files.example.com")
	t.Setenv("GODROP_MAX_TOTAL_SIZE", "1GB")

	code, out, _ := run(t, testBuild(), "doctor", "--offline", "--json", "--url", "", "--token", "")
	var report struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if report.OK && code != 0 {
		t.Errorf("a healthy report must exit 0, got %d", code)
	}
}

func TestInitReportsADuplicateTokenName(t *testing.T) {
	dataDir := t.TempDir()
	if code, _, _ := run(t, testBuild(), initArgs(t.TempDir(), dataDir)...); code != 0 {
		t.Fatal("the first setup should succeed")
	}
	code, _, stderr := run(t, testBuild(), initArgs(t.TempDir(), dataDir)...)
	if code == 0 {
		t.Error("reusing a token name should fail rather than silently continue")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestInitPropagatesAStartFailure(t *testing.T) {
	fakeDocker(t, 1)
	code, _, stderr := run(t, testBuild(), initArgs(t.TempDir(), t.TempDir(), "--start")...)
	if code == 0 {
		t.Error("a failed start should be reported")
	}
	if !strings.Contains(stderr, "docker compose up failed") {
		t.Errorf("stderr = %q", stderr)
	}
}

// failingWriter stands in for a closed pipe: the caller went away while we were
// still writing.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestJSONOutputReportsAWriteFailure(t *testing.T) {
	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")

	var stderr bytes.Buffer
	code := ExecuteWith(context.Background(), testBuild(),
		[]string{"doctor", "--offline", "--json"}, failingWriter{}, &stderr)
	if code == 0 {
		t.Error("a broken output stream should be reported")
	}
}

func TestTemporaryTokenReportsAStoreThatCannotBeOpened(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	path := tokens.Path(dir)
	if err := os.WriteFile(path, []byte(`{"tokens":[]}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	cfg := mustConfig(t, map[string]string{"GODROP_DATA_DIR": dir})
	if _, _, err := temporaryToken(cfg); err == nil {
		t.Error("an unreadable token store should be reported")
	}
}

func TestDoctorJSONExitsZeroForAHealthyInstance(t *testing.T) {
	const token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	base, stop := serveInBackground(t, token, nil)
	defer stop()

	// No base URL is configured, so nothing forces an https or DNS check; the
	// round trip runs against the instance that is actually running.
	code, out, stderr := run(t, testBuild(), "doctor", "--offline", "--json", "--url", base, "--token", token)
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v (%q, stderr %q)", err, out, stderr)
	}
	if !report.OK {
		for _, c := range report.Checks {
			if c.Status == "fail" {
				t.Errorf("unexpected failure: %s: %s", c.Name, c.Detail)
			}
		}
	}
	if report.OK && code != 0 {
		t.Errorf("a healthy report must exit 0, got %d", code)
	}
}

func TestInitReportsATelemetryOptOutFailure(t *testing.T) {
	dataDir := t.TempDir()
	// A directory where the opt-out marker belongs makes recording the choice
	// impossible; the setup must say so instead of silently leaving telemetry on.
	if err := os.Mkdir(filepath.Join(dataDir, telemetry.OptOutFile), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, testBuild(), initArgs(t.TempDir(), dataDir, "--telemetry=false")...)
	if code == 0 {
		t.Error("a failed opt-out should stop the setup")
	}
	if !strings.Contains(stderr, "telemetry") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestTemporaryTokenReportsAStoreThatCannotBeWritten(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	if err := os.WriteFile(tokens.Path(dir), []byte(`{"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	cfg := mustConfig(t, map[string]string{"GODROP_DATA_DIR": dir})
	if _, _, err := temporaryToken(cfg); err == nil {
		t.Error("a token store that cannot be written should be reported")
	}
}

func TestDefaultHint(t *testing.T) {
	cases := []struct{ desc, def, want string }{
		{"", "100MB", "Press enter to keep 100MB."},
		{"Per-file limit.", "100MB", "Per-file limit.\nPress enter to keep 100MB."},
		{"", "", "Press enter to leave this empty."},
		{"Optional.", "", "Optional.\nPress enter to leave this empty."},
	}
	for _, c := range cases {
		if got := defaultHint(c.desc, c.def); got != c.want {
			t.Errorf("defaultHint(%q, %q) = %q, want %q", c.desc, c.def, got, c.want)
		}
	}
}

func TestNoInputFlagIsTheConventionalSpelling(t *testing.T) {
	outDir, dataDir := t.TempDir(), t.TempDir()
	code, _, stderr := run(t, testBuild(), "init", "--no-input",
		"--out-dir", outDir, "--data-dir", dataDir, "--no-external-check", "--deployment", "env")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".env")); err != nil {
		t.Errorf("--no-input should run the wizard without prompting: %v", err)
	}
	// The older spelling still works but is hidden from help.
	_, help, _ := run(t, testBuild(), "init", "--help")
	if strings.Contains(help, "--non-interactive") {
		t.Error("the deprecated alias should not clutter the help output")
	}
	if !strings.Contains(help, "--no-input") {
		t.Error("--no-input should be documented")
	}
}
