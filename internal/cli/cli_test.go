package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

func testBuild() Build {
	return Build{Version: "1.2.3", Commit: "abc1234", Date: "2026-08-15", PostHogHost: "https://eu.i.posthog.com"}
}

// run drives the command line as a user would and returns exit code, stdout and
// stderr.
func run(t *testing.T, build Build, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := ExecuteWith(context.Background(), build, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, testBuild(), "version")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "godrop 1.2.3") || !strings.Contains(out, "abc1234") {
		t.Errorf("output = %q", out)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run(t, testBuild(), "version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out)
	}
	if got["version"] != "1.2.3" || got["commit"] != "abc1234" || got["date"] != "2026-08-15" {
		t.Errorf("json = %v", got)
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	code, out, _ := run(t, testBuild(), "--help")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, cmd := range []string{"serve", "init", "doctor", "token", "telemetry", "health", "version", "completion"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("help should list %q:\n%s", cmd, out)
		}
	}
}

func TestUnknownCommandFails(t *testing.T) {
	code, _, stderr := run(t, testBuild(), "frobnicate")
	if code == 0 {
		t.Error("an unknown command should fail")
	}
	if !strings.Contains(stderr, "error:") {
		t.Errorf("stderr = %q", stderr)
	}
}

// ------------------------------------------------------------------- tokens

func TestTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_TOKENS", "")

	code, out, _ := run(t, testBuild(), "token", "create", "--name", "claude-code", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	var created map[string]string
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("create output is not JSON: %v (%q)", err, out)
	}
	if !strings.HasPrefix(created["token"], tokens.Prefix) {
		t.Errorf("token = %q, want the gd_ prefix", created["token"])
	}
	if created["name"] != "claude-code" {
		t.Errorf("name = %q", created["name"])
	}

	code, out, _ = run(t, testBuild(), "token", "list", "--data-dir", dir)
	if code != 0 {
		t.Fatalf("list exit = %d", code)
	}
	if !strings.Contains(out, "claude-code") {
		t.Errorf("list output = %q", out)
	}
	if strings.Contains(out, created["token"]) {
		t.Fatal("the clear-text token must never be listed")
	}

	code, out, _ = run(t, testBuild(), "token", "revoke", "claude-code", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("revoke exit = %d", code)
	}
	if !strings.Contains(out, "claude-code") {
		t.Errorf("revoke output = %q", out)
	}

	code, _, stderr := run(t, testBuild(), "token", "revoke", "claude-code", "--data-dir", dir)
	if code == 0 {
		t.Error("revoking twice should fail")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestTokenCreateHumanOutputShowsTheTokenOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_TOKENS", "")
	code, out, _ := run(t, testBuild(), "token", "create", "--data-dir", dir)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"Token created", "only time", "curl -X POST"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
}

func TestTokenListWithNothingConfigured(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "")
	code, out, _ := run(t, testBuild(), "token", "list", "--data-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "No tokens yet") {
		t.Errorf("output = %q", out)
	}
}

func TestTokenListJSONIncludesEnvironmentCount(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "env-one,env-two")
	dir := t.TempDir()
	run(t, testBuild(), "token", "create", "--name", "file-one", "--data-dir", dir)
	code, out, _ := run(t, testBuild(), "token", "list", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got struct {
		Tokens []struct {
			Name     string `json:"name"`
			LastUsed string `json:"last_used"`
		} `json:"tokens"`
		EnvTokens int `json:"env_tokens"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if got.EnvTokens != 2 || len(got.Tokens) != 1 {
		t.Errorf("json = %+v", got)
	}
}

func TestTokenListMentionsEnvironmentTokens(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "env-one")
	dir := t.TempDir()
	run(t, testBuild(), "token", "create", "--name", "file-one", "--data-dir", dir)
	_, out, _ := run(t, testBuild(), "token", "list", "--data-dir", dir)
	if !strings.Contains(out, "GODROP_TOKENS") {
		t.Errorf("the listing should explain environment tokens:\n%s", out)
	}
}

func TestTokenRevokeExplainsEnvironmentTokens(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "env-one")
	_, _, stderr := run(t, testBuild(), "token", "revoke", "env-1", "--data-dir", t.TempDir())
	if !strings.Contains(stderr, "GODROP_TOKENS") {
		t.Errorf("stderr should explain where environment tokens live: %q", stderr)
	}
}

func TestTokenCreateRejectsBadNames(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "")
	code, _, stderr := run(t, testBuild(), "token", "create", "--name", "bad name", "--data-dir", t.TempDir())
	if code == 0 {
		t.Error("an invalid name should fail")
	}
	if !strings.Contains(stderr, "invalid token name") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestTokenCommandsReportStoreFailures(t *testing.T) {
	requireStrictPermissions(t)
	t.Setenv("GODROP_TOKENS", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, tokens.FileName), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"token", "create", "--data-dir", dir},
		{"token", "list", "--data-dir", dir},
		{"token", "revoke", "x", "--data-dir", dir},
	} {
		if code, _, _ := run(t, testBuild(), args...); code == 0 {
			t.Errorf("%v should fail on a corrupt token file", args)
		}
	}
}

func TestDataDirComesFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "")
	if code, _, _ := run(t, testBuild(), "token", "create", "--name", "from-env"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(tokens.Path(dir)); err != nil {
		t.Errorf("the token file should be created in GODROP_DATA_DIR: %v", err)
	}
}

func TestHumanSince(t *testing.T) {
	now := time.Now()
	cases := map[time.Duration]string{
		-10 * time.Second:     "just now",
		-5 * time.Minute:      "min ago",
		-3 * time.Hour:        "hours ago",
		-4 * 24 * time.Hour:   "days ago",
		-400 * 24 * time.Hour: "-",
	}
	for d, want := range cases {
		if got := humanSince(now.Add(d)); !strings.Contains(got, want) {
			t.Errorf("humanSince(%v) = %q, want it to contain %q", d, got, want)
		}
	}
}

// ---------------------------------------------------------------- telemetry

func TestTelemetrySwitch(t *testing.T) {
	dir := t.TempDir()
	build := testBuild()
	build.PostHogKey = "phc_test"

	code, out, _ := run(t, build, "telemetry", "off", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "telemetry off") {
		t.Fatalf("exit = %d, out = %q", code, out)
	}

	code, out, _ = run(t, build, "telemetry", "status", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "Telemetry: off") {
		t.Fatalf("status = %q", out)
	}
	if !strings.Contains(out, "godrop telemetry off") && !strings.Contains(out, "disabled") {
		t.Errorf("status should explain why it is off:\n%s", out)
	}

	code, out, _ = run(t, build, "telemetry", "on", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "telemetry on") {
		t.Fatalf("on = %q", out)
	}
	_, out, _ = run(t, build, "telemetry", "status", "--data-dir", dir)
	if !strings.Contains(out, "Telemetry: on") {
		t.Errorf("status = %q", out)
	}
}

func TestTelemetryStatusShowsTheExactPayload(t *testing.T) {
	build := testBuild()
	build.PostHogKey = "phc_test"
	code, out, _ := run(t, build, "telemetry", "status", "--data-dir", t.TempDir(), "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got struct {
		State   string `json:"state"`
		Payload struct {
			Event      string         `json:"event"`
			DistinctID string         `json:"distinct_id"`
			Properties map[string]any `json:"properties"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if got.State != "on" || got.Payload.Event != "heartbeat" {
		t.Errorf("status = %+v", got)
	}
	if len(got.Payload.Properties) != 4 {
		t.Errorf("properties = %v, want exactly version, os, arch and deploy", got.Payload.Properties)
	}
}

func TestTelemetryStatusWithoutAKey(t *testing.T) {
	code, out, _ := run(t, testBuild(), "telemetry", "status", "--data-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "off") || !strings.Contains(out, "built from source") {
		t.Errorf("a source build should explain that telemetry is inert:\n%s", out)
	}
}

func TestTelemetryStatusJSONWithoutAKey(t *testing.T) {
	code, out, _ := run(t, testBuild(), "telemetry", "status", "--data-dir", t.TempDir(), "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"state": "off"`) {
		t.Errorf("json = %q", out)
	}
}

func TestTelemetrySendNow(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	build := testBuild()
	build.PostHogKey = "phc_test"
	build.PostHogHost = srv.URL

	code, out, _ := run(t, build, "telemetry", "status", "--data-dir", t.TempDir(), "--send")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got != 1 {
		t.Errorf("the endpoint was called %d times, want 1", got)
	}
	if !strings.Contains(out, "test heartbeat delivered") {
		t.Errorf("output = %q", out)
	}
}

func TestTelemetrySendFailureIsReported(t *testing.T) {
	build := testBuild()
	build.PostHogKey = "phc_test"
	build.PostHogHost = "http://127.0.0.1:1"
	code, _, stderr := run(t, build, "telemetry", "status", "--data-dir", t.TempDir(), "--send")
	if code == 0 {
		t.Error("a failed send should exit non-zero")
	}
	if !strings.Contains(stderr, "send failed") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestTelemetryReportsWriteFailures(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if code, _, _ := run(t, testBuild(), "telemetry", "off", "--data-dir", dir); code == 0 {
		t.Error("an unwritable data directory should fail")
	}
}

// ------------------------------------------------------------------- doctor

func TestDoctorOfflineJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_BASE_URL", "https://files.example.com")

	code, out, _ := run(t, testBuild(), "doctor", "--offline", "--json")
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if len(report.Checks) == 0 {
		t.Fatal("the report should contain checks")
	}
	if report.OK && code != 0 {
		t.Errorf("a passing report should exit 0, got %d", code)
	}
	if !report.OK && code == 0 {
		t.Error("a failing report should exit non-zero")
	}
}

func TestDoctorHumanOutputGroupsChecks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")

	_, out, _ := run(t, testBuild(), "doctor", "--offline")
	for _, want := range []string{"Configuration", "Storage", "Security"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should have a %q section:\n%s", want, out)
		}
	}
}

func TestDoctorFailsOnBrokenConfiguration(t *testing.T) {
	t.Setenv("GODROP_MAX_FILE_SIZE", "lots")
	code, out, _ := run(t, testBuild(), "doctor", "--offline")
	if code == 0 {
		t.Error("a broken configuration should exit non-zero")
	}
	if !strings.Contains(out, "check(s) failed") {
		t.Errorf("output = %q", out)
	}
}

func TestDoctorAgainstARemoteInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer srv.Close()
	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")

	code, out, _ := run(t, testBuild(), "doctor", "--offline", "--url", srv.URL, "--token", "wrong")
	if code == 0 {
		t.Error("a remote instance rejecting the token should fail the report")
	}
	if !strings.Contains(out, "upload") {
		t.Errorf("output should mention the failed round trip:\n%s", out)
	}
}

func TestDoctorMintsATemporaryToken(t *testing.T) {
	// Without GODROP_TOKENS the doctor creates a throwaway token so the round
	// trip can run, then removes it again.
	dir := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "")

	if code, _, _ := run(t, testBuild(), "doctor", "--offline"); code > 1 {
		t.Fatalf("exit = %d", code)
	}
	store, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range store.List() {
		if strings.HasPrefix(tok.Name, "doctor-") {
			t.Errorf("the temporary token %q was left behind", tok.Name)
		}
	}
}

// ------------------------------------------------------------------- health

func TestHealthCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	code, out, _ := run(t, testBuild(), "health", "--url", srv.URL+"/healthz")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "healthy") {
		t.Errorf("output = %q", out)
	}

	code, out, _ = run(t, testBuild(), "health", "--url", srv.URL+"/healthz", "--json")
	if code != 0 || !strings.Contains(out, `"status": "ok"`) {
		t.Errorf("json output = %q", out)
	}
}

func TestHealthCommandFailures(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()

	if code, _, _ := run(t, testBuild(), "health", "--url", failing.URL); code == 0 {
		t.Error("an unhealthy instance should exit non-zero")
	}
	if code, _, _ := run(t, testBuild(), "health", "--url", "http://127.0.0.1:1"); code == 0 {
		t.Error("an unreachable instance should exit non-zero")
	}
}

func TestLocalHealthURL(t *testing.T) {
	cases := map[string]string{
		"":              "http://127.0.0.1:8080/healthz",
		":9000":         "http://127.0.0.1:9000/healthz",
		"0.0.0.0:9000":  "http://127.0.0.1:9000/healthz",
		"127.0.0.1:123": "http://127.0.0.1:123/healthz",
		"9999":          "http://127.0.0.1:9999/healthz",
	}
	for addr, want := range cases {
		t.Setenv("GODROP_ADDR", addr)
		if got := localHealthURL(); got != want {
			t.Errorf("GODROP_ADDR=%q gives %q, want %q", addr, got, want)
		}
	}
}

// requireStrictPermissions skips a test that depends on POSIX permission
// semantics. As root every mode is writable anyway, and on Windows chmod only
// toggles a read-only bit, so the situations these tests create cannot exist.
func requireStrictPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}

// requirePOSIXModes skips a test that asserts exact file modes. Windows has no
// POSIX permission bits, so a file created with 0600 does not report 0600.
func requirePOSIXModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not POSIX bits on Windows")
	}
}
