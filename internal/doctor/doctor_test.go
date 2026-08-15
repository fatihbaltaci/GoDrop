package doctor

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/netcheck"
	"github.com/fatihbaltaci/GoDrop/internal/server"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

// A realistic token: the strength heuristic flags placeholders such as
// "changeme" or anything with "token" in it, which a generated gd_ value never
// has.
const testToken = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"

// find returns a check by name, failing the test when it is missing.
func find(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing from the report: %+v", name, report.Checks)
	return Check{}
}

func has(report Report, name string) bool {
	for _, c := range report.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}

func baseConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "GODROP_TOKENS":
			return testToken
		case "GODROP_DATA_DIR":
			return dir
		case "GODROP_MAX_TOTAL_SIZE":
			return "1GB"
		case "GODROP_BASE_URL":
			return "https://files.example.com"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// offlineOptions diagnoses only what can be inspected locally.
func offlineOptions(cfg *config.Config) Options {
	return Options{
		Config:  cfg,
		Version: "1.0.0",
		Offline: true,
		Runner:  noFirewall,
		Now:     func() time.Time { return time.Now().UTC() },
	}
}

func noFirewall(context.Context, string, ...string) (string, error) {
	return "", errors.New("not installed")
}

func TestHealthyInstallationPasses(t *testing.T) {
	cfg := baseConfig(t)
	if _, err := storage.New(cfg.DataDir, cfg.MaxTotalSize); err != nil {
		t.Fatal(err)
	}
	report := Run(context.Background(), offlineOptions(cfg))

	if report.Failed() {
		for _, c := range report.Checks {
			if c.Status == Fail {
				t.Errorf("unexpected failure: %s — %s", c.Name, c.Detail)
			}
		}
	}
	for _, name := range []string{"tokens", "base_url", "max_file_size", "storage_quota", "writable", "usage", "orphans"} {
		if got := find(t, report, name); got.Status == Fail {
			t.Errorf("%s = %s (%s)", name, got.Status, got.Detail)
		}
	}
	if report.Version != "1.0.0" {
		t.Errorf("version = %q", report.Version)
	}
}

func TestConfigurationErrorsAreReportedFirst(t *testing.T) {
	report := Run(context.Background(), Options{
		ConfigErr: errors.New("GODROP_MAX_FILE_SIZE: invalid size \"lots\""),
		Offline:   true,
	})
	c := find(t, report, "configuration")
	if c.Status != Fail || !strings.Contains(c.Detail, "GODROP_MAX_FILE_SIZE") {
		t.Errorf("configuration check = %+v", c)
	}
	if !report.Failed() {
		t.Error("a broken configuration must fail the report")
	}
	if has(report, "writable") {
		t.Error("storage cannot be diagnosed without a configuration")
	}
}

func TestRemoteDiagnosisSkipsLocalChecks(t *testing.T) {
	report := Run(context.Background(), Options{Offline: true, TargetURL: "https://files.example.com"})
	if c := find(t, report, "configuration"); c.Status != Skip {
		t.Errorf("configuration = %+v, want skip when diagnosing a remote instance", c)
	}
}

func TestMissingBaseURLIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = ""
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "base_url")
	if c.Status != Warn || !strings.Contains(c.Fix, "GODROP_BASE_URL") {
		t.Errorf("base_url = %+v", c)
	}
}

func TestMalformedBaseURLFails(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "files.example.com"
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "base_url")
	if c.Status != Fail {
		t.Errorf("base_url = %+v, want a failure", c)
	}
}

func TestMissingQuotaIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	cfg.MaxTotalSize = 0
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "storage_quota")
	if c.Status != Warn || !strings.Contains(c.Fix, "GODROP_MAX_TOTAL_SIZE") {
		t.Errorf("storage_quota = %+v", c)
	}
}

func TestNearlyFullQuotaIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	cfg.MaxTotalSize = 10
	st, err := storage.New(cfg.DataDir, cfg.MaxTotalSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("txt", strings.NewReader("1234567890"), 100); err != nil {
		t.Fatal(err)
	}
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "usage")
	if c.Status != Warn {
		t.Errorf("usage = %+v, want a warning when the quota is nearly full", c)
	}
}

func TestMissingDataDirectoryIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "not-created-yet")
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "data_dir")
	if c.Status != Warn {
		t.Errorf("data_dir = %+v, want a warning", c)
	}
}

func TestDataDirectoryThatIsAFileFails(t *testing.T) {
	cfg := baseConfig(t)
	path := filepath.Join(cfg.DataDir, "afile")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = path
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "data_dir")
	if c.Status != Fail || !strings.Contains(c.Detail, "not a directory") {
		t.Errorf("data_dir = %+v", c)
	}
}

func TestUnreadableDataDirectoryFails(t *testing.T) {
	requireStrictPermissions(t)
	cfg := baseConfig(t)
	nested := filepath.Join(cfg.DataDir, "nested")
	if err := os.MkdirAll(nested, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o700) })
	cfg.DataDir = filepath.Join(nested, "data")

	c := find(t, Run(context.Background(), offlineOptions(cfg)), "data_dir")
	if c.Status != Fail {
		t.Errorf("data_dir = %+v, want a failure", c)
	}
}

func TestReadOnlyDataDirectoryFails(t *testing.T) {
	requireStrictPermissions(t)
	cfg := baseConfig(t)
	if _, err := storage.New(cfg.DataDir, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.DataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.DataDir, 0o700) })

	report := Run(context.Background(), offlineOptions(cfg))
	if c := find(t, report, "writable"); c.Status != Fail {
		t.Errorf("writable = %+v, want a failure", c)
	}
	if !strings.Contains(find(t, report, "writable").Fix, "-v godrop-data:/data") {
		t.Error("the fix should mention mounting a volume, the usual cause")
	}
}

func TestOrphansAreReported(t *testing.T) {
	cfg := baseConfig(t)
	st, err := storage.New(cfg.DataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.Create("txt", strings.NewReader("real"), 100)
	if err != nil {
		t.Fatal(err)
	}
	// Beside a stored file, not in the root: files in the root belong to the
	// service itself and are not uploads gone astray.
	if err := os.WriteFile(filepath.Join(filepath.Dir(stored.Path), "stray.txt"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "orphans")
	if c.Status != Warn || !strings.Contains(c.Detail, "stray.txt") {
		t.Errorf("orphans = %+v", c)
	}
}

func TestWeakTokensAreReported(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Tokens = []string{"changeme", "short"}
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "token_strength")
	if c.Status != Fail || !strings.Contains(c.Fix, "godrop token create") {
		t.Errorf("token_strength = %+v", c)
	}
}

func TestWorldReadableDataDirectoryIsAWarning(t *testing.T) {
	requirePOSIXModes(t)
	requireStrictPermissions(t)
	cfg := baseConfig(t)
	if err := os.Chmod(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "data_dir_perms")
	if c.Status != Warn || !strings.Contains(c.Fix, "chmod 700") {
		t.Errorf("data_dir_perms = %+v", c)
	}
}

func TestTokenFilePermissionsAreChecked(t *testing.T) {
	requirePOSIXModes(t)
	cfg := baseConfig(t)
	store, err := tokens.New(tokens.Path(cfg.DataDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("x"); err != nil {
		t.Fatal(err)
	}
	if c := find(t, Run(context.Background(), offlineOptions(cfg)), "token_file_perms"); c.Status != Pass {
		t.Errorf("token_file_perms = %+v, want pass for a freshly written file", c)
	}

	if err := os.Chmod(tokens.Path(cfg.DataDir), 0o644); err != nil {
		t.Fatal(err)
	}
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "token_file_perms")
	if c.Status != Fail || !strings.Contains(c.Fix, "chmod 600") {
		t.Errorf("token_file_perms = %+v", c)
	}
}

func TestEnvFileIsChecked(t *testing.T) {
	requirePOSIXModes(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("GODROP_TOKENS=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(t)
	opts := offlineOptions(cfg)
	opts.WorkDir = dir

	c := find(t, Run(context.Background(), opts), "env_file_perms")
	if c.Status != Fail || !strings.Contains(c.Fix, "chmod 600") {
		t.Errorf("env_file_perms = %+v", c)
	}

	if err := os.Chmod(envPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if c := find(t, Run(context.Background(), opts), "env_file_perms"); c.Status != Pass {
		t.Errorf("env_file_perms = %+v, want pass at 0600", c)
	}
}

func TestEnvFileTrackedByGitIsReported(t *testing.T) {
	original := gitTracked
	gitTracked = func(string, string) bool { return true }
	t.Cleanup(func() { gitTracked = original })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := offlineOptions(baseConfig(t))
	opts.WorkDir = dir

	c := find(t, Run(context.Background(), opts), "env_in_git")
	if c.Status != Fail || !strings.Contains(c.Fix, "git rm --cached") {
		t.Errorf("env_in_git = %+v", c)
	}
}

func TestMissingEnvFileIsNotChecked(t *testing.T) {
	opts := offlineOptions(baseConfig(t))
	opts.WorkDir = t.TempDir()
	if has(Run(context.Background(), opts), "env_file_perms") {
		t.Error("there is nothing to say about an absent .env")
	}
}

func TestPlainHTTPBaseURLFails(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "http://files.example.com"
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "https")
	if c.Status != Fail || !strings.Contains(c.Detail, "clear text") {
		t.Errorf("https = %+v", c)
	}
}

func TestLocalHTTPBaseURLIsAccepted(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "http://localhost:8080"
	if c := find(t, Run(context.Background(), offlineOptions(cfg)), "https"); c.Status == Fail {
		t.Errorf("a local URL should not be flagged: %+v", c)
	}
}

func TestWideOpenCORSIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "cors")
	if c.Status != Warn {
		t.Errorf("cors = %+v, want a warning for the default *", c)
	}

	cfg.CORSOrigins = []string{"https://app.example.com"}
	if has(Run(context.Background(), offlineOptions(cfg)), "cors") {
		t.Error("a restricted CORS setting needs no comment")
	}
}

func TestPersistenceIsCheckedInsideContainers(t *testing.T) {
	originalContainer := inContainer
	inContainer = func() bool { return true }
	t.Cleanup(func() { inContainer = originalContainer })

	cfg := baseConfig(t)
	report := Run(context.Background(), offlineOptions(cfg))
	// On a machine without /proc/mounts the check is silently skipped; where it
	// does exist, a temporary directory is never a mounted volume.
	if c, ok := lookup(report, "persistence"); ok && c.Status != Fail {
		t.Errorf("persistence = %+v, want a failure for a container-local path", c)
	}
}

func lookup(report Report, name string) (Check, bool) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// ------------------------------------------------------------------ network

// newGoDrop starts a real GoDrop instance for the round-trip checks.
func newGoDrop(t *testing.T) (*httptest.Server, *config.Config) {
	t.Helper()
	cfg := baseConfig(t)
	cfg.BaseURL = ""
	store, err := storage.New(cfg.DataDir, cfg.MaxTotalSize)
	if err != nil {
		t.Fatal(err)
	}
	tokenStore, err := tokens.New(tokens.Path(cfg.DataDir), cfg.Tokens)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Options{
		Config: cfg, Store: store, Tokens: tokenStore, Version: "test",
	}))
	t.Cleanup(srv.Close)
	return srv, cfg
}

// probe serves the reachability check, standing in for godrop.sh.
func probe(t *testing.T, result netcheck.ExternalResult) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEndToEndRoundTripAgainstARealServer(t *testing.T) {
	godrop, cfg := newGoDrop(t)
	checker := probe(t, netcheck.ExternalResult{OK: true, Status: 200, Location: "FRA"})

	report := Run(context.Background(), Options{
		Config:    cfg,
		Version:   "1.0.0",
		TargetURL: godrop.URL,
		Token:     testToken,
		CheckURL:  checker.URL,
		Runner:    noFirewall,
		HTTP:      godrop.Client(),
	})

	for _, name := range []string{"upload", "download", "delete"} {
		if c := find(t, report, name); c.Status != Pass {
			t.Errorf("%s = %+v, want pass", name, c)
		}
	}
	if c := find(t, report, "external"); c.Status != Pass {
		t.Errorf("external = %+v", c)
	}
	// The file used for the check must not be left behind.
	if files, _ := mustStore(t, cfg).Stats(); files != 0 {
		t.Errorf("the round trip left %d file(s) behind", files)
	}
}

func mustStore(t *testing.T, cfg *config.Config) *storage.Store {
	t.Helper()
	st, err := storage.New(cfg.DataDir, cfg.MaxTotalSize)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestEndToEndDetectsABadToken(t *testing.T) {
	godrop, cfg := newGoDrop(t)
	report := Run(context.Background(), Options{
		Config: cfg, TargetURL: godrop.URL, Token: "wrong", Offline: true,
		Runner: noFirewall, HTTP: godrop.Client(),
	})
	c := find(t, report, "upload")
	if c.Status != Fail || !strings.Contains(c.Detail, "401") {
		t.Errorf("upload = %+v, want a 401 failure", c)
	}
}

func TestEndToEndDetectsAProxyBodyLimit(t *testing.T) {
	// A proxy that answers 413 with an HTML error page, before GoDrop sees the
	// request — the classic nginx client_max_body_size trap.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte("<html><body>413 Request Entity Too Large</body></html>"))
	}))
	defer proxy.Close()

	report := Run(context.Background(), Options{
		TargetURL: proxy.URL, Token: testToken, Offline: true, Runner: noFirewall,
	})
	c := find(t, report, "proxy_body_limit")
	if c.Status != Fail || !strings.Contains(c.Fix, "client_max_body_size") {
		t.Errorf("proxy_body_limit = %+v", c)
	}
}

func TestEndToEndDetectsAlteredContent(t *testing.T) {
	// A proxy that rewrites bodies would corrupt every download.
	godrop, cfg := newGoDrop(t)
	rewriting := &http.Client{Transport: rewriteBody{godrop.Client().Transport}}

	report := Run(context.Background(), Options{
		Config: cfg, TargetURL: godrop.URL, Token: testToken, Offline: true,
		Runner: noFirewall, HTTP: rewriting,
	})
	c := find(t, report, "download")
	if c.Status != Fail || !strings.Contains(c.Detail, "differ") {
		t.Errorf("download = %+v, want a mismatch failure", c)
	}
}

func TestEndToEndSkipsWithoutCredentials(t *testing.T) {
	report := Run(context.Background(), Options{Offline: true, Runner: noFirewall})
	if c := find(t, report, "round_trip"); c.Status != Skip {
		t.Errorf("round_trip = %+v, want skip", c)
	}
}

func TestEndToEndReportsAnUnreachableServer(t *testing.T) {
	report := Run(context.Background(), Options{
		TargetURL: "http://127.0.0.1:1", Token: testToken, Offline: true, Runner: noFirewall,
	})
	if c := find(t, report, "upload"); c.Status != Fail {
		t.Errorf("upload = %+v, want a failure", c)
	}
}

func TestEndToEndReportsAnUnexpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	report := Run(context.Background(), Options{
		TargetURL: srv.URL, Token: testToken, Offline: true, Runner: noFirewall,
	})
	c := find(t, report, "upload")
	if c.Status != Fail || !strings.Contains(c.Detail, "unexpected response") {
		t.Errorf("upload = %+v", c)
	}
}

func TestFirewallStatesAreReported(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "http://127.0.0.1:8080"

	tests := []struct {
		name   string
		runner netcheck.Runner
		want   Status
	}{
		{"open", fixedRunner("ufw", "Status: active\n8080/tcp ALLOW Anywhere\n"), Pass},
		{"blocked", fixedRunner("ufw", "Status: active\n22/tcp ALLOW Anywhere\n"), Fail},
		{"not inspectable", noFirewall, Skip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := offlineOptions(cfg)
			opts.Runner = tt.runner
			c := find(t, Run(context.Background(), opts), "firewall")
			if c.Status != tt.want {
				t.Errorf("firewall = %+v, want %s", c, tt.want)
			}
		})
	}
}

func fixedRunner(command, output string) netcheck.Runner {
	return func(_ context.Context, name string, _ ...string) (string, error) {
		if name == command {
			return output, nil
		}
		return "", errors.New("not installed")
	}
}

func TestListeningIsReported(t *testing.T) {
	godrop, cfg := newGoDrop(t)
	cfg.Addr = strings.TrimPrefix(godrop.URL, "http://")
	cfg.BaseURL = godrop.URL

	opts := offlineOptions(cfg)
	if c := find(t, Run(context.Background(), opts), "listening"); c.Status != Pass {
		t.Errorf("listening = %+v, want pass for a running server", c)
	}

	cfg.Addr = "127.0.0.1:1"
	c := find(t, Run(context.Background(), opts), "listening")
	if c.Status != Warn || !strings.Contains(c.Fix, "docker compose up") {
		t.Errorf("listening = %+v, want a warning with a hint", c)
	}
}

func TestUnreachableFromTheInternetFails(t *testing.T) {
	checker := probe(t, netcheck.ExternalResult{OK: false, Status: 502, Error: "connection refused"})
	cfg := baseConfig(t)
	cfg.BaseURL = "http://127.0.0.1:8080"

	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: checker.URL, HTTP: checker.Client(),
	})
	c := find(t, report, "external")
	if c.Status != Fail || !strings.Contains(c.Fix, "security group") {
		t.Errorf("external = %+v", c)
	}
}

func TestUnreachableWithoutADetailedReason(t *testing.T) {
	checker := probe(t, netcheck.ExternalResult{OK: false, Status: 503})
	cfg := baseConfig(t)
	cfg.BaseURL = "http://127.0.0.1:8080"

	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: checker.URL, HTTP: checker.Client(),
	})
	if c := find(t, report, "external"); !strings.Contains(c.Detail, "503") {
		t.Errorf("external = %+v, want the status code in the detail", c)
	}
}

func TestReachabilityServiceOutageIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "http://127.0.0.1:8080"
	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1",
	})
	c := find(t, report, "external")
	if c.Status != Warn || !strings.Contains(c.Fix, "curl -sI") {
		t.Errorf("external = %+v, want a warning with a manual fallback", c)
	}
}

func TestDNSFailureIsReported(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "https://godrop-nonexistent.invalid"
	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1",
	})
	c := find(t, report, "dns")
	if c.Status != Fail || !strings.Contains(c.Fix, "A record") {
		t.Errorf("dns = %+v", c)
	}
}

func TestMalformedTargetURLIsReported(t *testing.T) {
	report := Run(context.Background(), Options{
		TargetURL: "::not a url::", Runner: noFirewall,
	})
	if c := find(t, report, "dns"); c.Status != Fail {
		t.Errorf("dns = %+v, want a failure for an unparsable URL", c)
	}
}

func TestNoTargetMeansNoNetworkChecks(t *testing.T) {
	report := Run(context.Background(), Options{Offline: true, Runner: noFirewall})
	if c := find(t, report, "reachability"); c.Status != Skip {
		t.Errorf("reachability = %+v, want skip", c)
	}
}

func TestOfflineSkipsExternalAndVersionChecks(t *testing.T) {
	cfg := baseConfig(t)
	report := Run(context.Background(), offlineOptions(cfg))
	if c := find(t, report, "external"); c.Status != Skip {
		t.Errorf("external = %+v, want skip in offline mode", c)
	}
	if c := find(t, report, "update"); c.Status != Skip {
		t.Errorf("update = %+v, want skip in offline mode", c)
	}
}

func TestTLSIsInspectedForHTTPSTargets(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsSrv.Close()

	cfg := baseConfig(t)
	cfg.BaseURL = tlsSrv.URL
	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1", HTTP: tlsSrv.Client(),
	})
	// httptest serves an untrusted certificate, which is a genuine failure.
	if c := find(t, report, "tls"); c.Status != Fail {
		t.Errorf("tls = %+v, want a failure for an untrusted certificate", c)
	}
}

// ------------------------------------------------------------------ version

func TestVersionCheckReportsAnUpdate(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v2.0.0"})
	}))
	defer github.Close()

	report := Run(context.Background(), Options{
		Version: "v1.0.0",
		Runner:  noFirewall,
		HTTP:    &http.Client{Transport: redirectTo(github.URL)},
	})
	c := find(t, report, "update")
	if c.Status != Warn || !strings.Contains(c.Detail, "v2.0.0") {
		t.Errorf("update = %+v", c)
	}
	if !strings.Contains(c.Fix, "godrop.sh/install.sh") {
		t.Error("the fix should be the install command")
	}
}

func TestVersionCheckPassesWhenCurrent(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v1.0.0"})
	}))
	defer github.Close()

	report := Run(context.Background(), Options{
		Version: "1.0.0", Runner: noFirewall,
		HTTP: &http.Client{Transport: redirectTo(github.URL)},
	})
	if c := find(t, report, "update"); c.Status != Pass {
		t.Errorf("update = %+v, want pass", c)
	}
}

func TestDevelopmentBuildsAreNotNagged(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v9.9.9"})
	}))
	defer github.Close()

	report := Run(context.Background(), Options{
		Version: "dev", Runner: noFirewall,
		HTTP: &http.Client{Transport: redirectTo(github.URL)},
	})
	if c := find(t, report, "update"); c.Status != Pass {
		t.Errorf("update = %+v, want a development build to pass", c)
	}
}

func TestVersionCheckSkipsOnFailure(t *testing.T) {
	report := Run(context.Background(), Options{
		Version: "1.0.0", Runner: noFirewall,
		HTTP: &http.Client{Transport: redirectTo("http://127.0.0.1:1")},
	})
	if c := find(t, report, "update"); c.Status != Skip {
		t.Errorf("update = %+v, want skip when GitHub cannot be reached", c)
	}
}

func TestLatestReleaseErrors(t *testing.T) {
	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if _, err := LatestRelease(context.Background(), &http.Client{Transport: redirectTo(srv.URL)}); err == nil {
			t.Error("a rate-limited API should be reported")
		}
	})
	t.Run("garbage body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		if _, err := LatestRelease(context.Background(), &http.Client{Transport: redirectTo(srv.URL)}); err == nil {
			t.Error("an unparsable body should be reported")
		}
	})
	t.Run("bad context", func(t *testing.T) {
		//lint:ignore SA1012 exercising the guard
		if _, err := LatestRelease(nil, http.DefaultClient); err == nil { //nolint:staticcheck
			t.Error("an unusable context should be reported")
		}
	})
}

// ------------------------------------------------------------------ helpers

func redirectTo(base string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		u := *r.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(base, "http://")
		clone := r.Clone(r.Context())
		clone.URL = &u
		return http.DefaultTransport.RoundTrip(clone)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// rewriteBody corrupts download responses, imitating a proxy that rewrites
// content.
type rewriteBody struct{ base http.RoundTripper }

func (t rewriteBody) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(r)
	if err != nil || r.Method != http.MethodGet {
		return resp, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader("tampered"))
	resp.ContentLength = int64(len("tampered"))
	return resp, nil
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

// ------------------------------------------------------- injected conditions

func withDiskFree(t *testing.T, free, total int64, ok bool) {
	t.Helper()
	original := diskFree
	diskFree = func(string) (int64, int64, bool) { return free, total, ok }
	t.Cleanup(func() { diskFree = original })
}

func TestDiskSpaceThresholds(t *testing.T) {
	tests := []struct {
		name string
		free int64
		want Status
	}{
		{"plenty", 50 << 30, Pass},
		{"getting tight", 512 << 20, Warn},
		{"critical", 10 << 20, Fail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDiskFree(t, tt.free, 100<<30, true)
			c := find(t, Run(context.Background(), offlineOptions(baseConfig(t))), "disk_space")
			if c.Status != tt.want {
				t.Errorf("disk_space = %+v, want %s", c, tt.want)
			}
		})
	}
}

func TestDiskSpaceIsSkippedWhenUnavailable(t *testing.T) {
	withDiskFree(t, 0, 0, false)
	if has(Run(context.Background(), offlineOptions(baseConfig(t))), "disk_space") {
		t.Error("a platform without statfs should simply omit the check")
	}
}

func TestStatfsFailsForAMissingPath(t *testing.T) {
	if _, _, ok := statfs(filepath.Join(t.TempDir(), "nope")); ok {
		t.Error("statfs should report failure for a path that does not exist")
	}
}

func TestPersistenceDetectsAMountedVolume(t *testing.T) {
	originalContainer, originalMounts := inContainer, readMounts
	inContainer = func() bool { return true }
	t.Cleanup(func() { inContainer, readMounts = originalContainer, originalMounts })

	cfg := baseConfig(t)
	readMounts = func() ([]byte, error) {
		return []byte("proc /proc proc rw 0 0\n/dev/vdb " + cfg.DataDir + " ext4 rw 0 0\n"), nil
	}
	if c := find(t, Run(context.Background(), offlineOptions(cfg)), "persistence"); c.Status != Pass {
		t.Errorf("persistence = %+v, want pass for a mounted data directory", c)
	}
}

func TestPersistenceDetectsAMissingVolume(t *testing.T) {
	originalContainer, originalMounts := inContainer, readMounts
	inContainer = func() bool { return true }
	readMounts = func() ([]byte, error) { return []byte("overlay / overlay rw 0 0\n"), nil }
	t.Cleanup(func() { inContainer, readMounts = originalContainer, originalMounts })

	c := find(t, Run(context.Background(), offlineOptions(baseConfig(t))), "persistence")
	if c.Status != Fail || !strings.Contains(c.Fix, "-v godrop-data:/data") {
		t.Errorf("persistence = %+v, want a failure that explains the fix", c)
	}
}

func TestPersistenceIsSkippedWhenMountsCannotBeRead(t *testing.T) {
	originalContainer, originalMounts := inContainer, readMounts
	inContainer = func() bool { return true }
	readMounts = func() ([]byte, error) { return nil, errors.New("no /proc") }
	t.Cleanup(func() { inContainer, readMounts = originalContainer, originalMounts })

	if has(Run(context.Background(), offlineOptions(baseConfig(t))), "persistence") {
		t.Error("without mount information the check should stay silent")
	}
}

func TestInContainerAndGitTrackedHaveWorkingDefaults(t *testing.T) {
	// Just exercise the real implementations; the value depends on the machine.
	_ = inContainer()
	if gitTracked(t.TempDir(), "definitely-not-tracked.txt") {
		t.Error("a file in a fresh temporary directory cannot be tracked by git")
	}
}

func TestPrivilegeCheckReportsTheCurrentUser(t *testing.T) {
	if os.Geteuid() < 0 {
		// Windows has no effective uid, so there is nothing to report there.
		t.Skip("no effective user id on this platform")
	}
	c := find(t, Run(context.Background(), offlineOptions(baseConfig(t))), "privileges")
	if os.Geteuid() == 0 && c.Status != Warn {
		t.Errorf("running as root should warn, got %+v", c)
	}
	if os.Geteuid() > 0 && c.Status != Pass {
		t.Errorf("an unprivileged user should pass, got %+v", c)
	}
}

// --------------------------------------------------------------- DNS vs IP

// ipTrace serves the public-IP lookup with a fixed answer.
func ipTrace(t *testing.T, ip string) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fl=abc\nip=" + ip + "\nts=1\n"))
	}))
	t.Cleanup(srv.Close)
	return &http.Client{Transport: traceOnly{base: srv.URL}}
}

// traceOnly redirects only the Cloudflare trace lookup; everything else fails,
// which keeps these tests off the network.
type traceOnly struct{ base string }

func (t traceOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.Contains(r.URL.Path, "cdn-cgi/trace") {
		return nil, errors.New("blocked in tests")
	}
	u := *r.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(t.base, "http://")
	clone := r.Clone(r.Context())
	clone.URL = &u
	return http.DefaultTransport.RoundTrip(clone)
}

func TestDNSPointingAtThisMachinePasses(t *testing.T) {
	// An IP literal "resolves" to itself, so the comparison can run without DNS.
	cfg := baseConfig(t)
	cfg.BaseURL = "http://93.184.216.34:8080"

	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, HTTP: ipTrace(t, "93.184.216.34"), CheckURL: "http://127.0.0.1:1",
	})
	if c := find(t, report, "dns_points_here"); c.Status != Pass {
		t.Errorf("dns_points_here = %+v, want pass", c)
	}
}

func TestDNSPointingElsewhereIsAWarning(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "http://93.184.216.34:8080"

	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, HTTP: ipTrace(t, "5.9.1.2"), CheckURL: "http://127.0.0.1:1",
	})
	c := find(t, report, "dns_points_here")
	if c.Status != Warn || !strings.Contains(c.Fix, "Cloudflare") {
		t.Errorf("dns_points_here = %+v, want a warning that explains the usual reason", c)
	}
}

func TestLocalHostnamesSkipDNSChecks(t *testing.T) {
	cfg := baseConfig(t)
	cfg.BaseURL = "http://localhost:8080"
	report := Run(context.Background(), Options{Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1"})
	if has(report, "dns") {
		t.Error("a local hostname needs no DNS diagnosis")
	}
}

// ------------------------------------------------------------ TLS and e2e

func TestTLSExpiryWarning(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsSrv.Close()

	cfg := baseConfig(t)
	cfg.BaseURL = tlsSrv.URL
	// Just before expiry the certificate is still valid but should be flagged.
	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1",
		Now: func() time.Time { return tlsSrv.Certificate().NotAfter.Add(-24 * time.Hour) },
	})
	if c := find(t, report, "tls"); c.Status == Pass {
		t.Errorf("tls = %+v, want at least a warning close to expiry", c)
	}
}

func TestEndToEndReportsADeleteFailure(t *testing.T) {
	srv := failingDeleteServer(t)
	report := Run(context.Background(), Options{
		TargetURL: srv.URL, Token: testToken, Offline: true, Runner: noFirewall,
	})
	if c := find(t, report, "delete"); c.Status != Fail {
		t.Errorf("delete = %+v, want a failure", c)
	}
}

// failingDeleteServer accepts an upload and a download but refuses to delete.
func failingDeleteServer(t *testing.T) *httptest.Server {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body = "godrop doctor round trip\n"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url":   "http://" + r.Host + "/f/x",
				"files": []map[string]any{{"url": "http://" + r.Host + "/f/x", "size": len(body)}},
			})
		case http.MethodGet:
			_, _ = io.WriteString(w, body)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEndToEndReportsAnUnusableUploadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "://not-a-url"})
	}))
	defer srv.Close()

	report := Run(context.Background(), Options{
		TargetURL: srv.URL, Token: testToken, Offline: true, Runner: noFirewall,
	})
	if c := find(t, report, "download"); c.Status != Fail {
		t.Errorf("download = %+v, want a failure for an unusable URL", c)
	}
}

func TestEndToEndReportsAnUnreadableDownload(t *testing.T) {
	godrop, cfg := newGoDrop(t)
	client := &http.Client{Transport: brokenBody{godrop.Client().Transport}}
	report := Run(context.Background(), Options{
		Config: cfg, TargetURL: godrop.URL, Token: testToken, Offline: true,
		Runner: noFirewall, HTTP: client,
	})
	if c := find(t, report, "download"); c.Status != Fail {
		t.Errorf("download = %+v, want a failure when the body cannot be read", c)
	}
}

type brokenBody struct{ base http.RoundTripper }

func (t brokenBody) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(r)
	if err != nil || r.Method != http.MethodGet {
		return resp, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(errorReader{})
	return resp, nil
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestEndToEndReportsAnUnusableTargetURL(t *testing.T) {
	report := Run(context.Background(), Options{
		TargetURL: "http://\x7f", Token: testToken, Offline: true, Runner: noFirewall,
	})
	if c := find(t, report, "round_trip"); c.Status != Fail {
		t.Errorf("round_trip = %+v, want a failure for an unusable URL", c)
	}
}

func TestExternalSuccessWithoutALocation(t *testing.T) {
	checker := probe(t, netcheck.ExternalResult{OK: true, Status: 200})
	cfg := baseConfig(t)
	cfg.BaseURL = "http://127.0.0.1:8080"
	report := Run(context.Background(), Options{
		Config: cfg, Runner: noFirewall, CheckURL: checker.URL, HTTP: checker.Client(),
	})
	if c := find(t, report, "external"); c.Status != Pass {
		t.Errorf("external = %+v", c)
	}
}

func TestHelpers(t *testing.T) {
	if got := localAddr(":8080"); got != "127.0.0.1:8080" {
		t.Errorf("localAddr(:8080) = %q", got)
	}
	if got := localAddr("0.0.0.0:9000"); got != "127.0.0.1:9000" {
		t.Errorf("localAddr = %q", got)
	}
	if got := localAddr("[::]:9000"); got != "127.0.0.1:9000" {
		t.Errorf("localAddr = %q", got)
	}
	if got := localAddr("not-an-address"); got != "not-an-address" {
		t.Errorf("localAddr passthrough = %q", got)
	}
	if got := portOf(":8080", "not a url"); got != 8080 {
		t.Errorf("portOf fell back to %d", got)
	}
	if got := portOf("no-port", "also-not-a-url"); got != 0 {
		t.Errorf("portOf = %d, want 0 when nothing can be determined", got)
	}
	if !isLocalHost("127.0.0.1") || !isLocalHost("10.1.2.3") || !isLocalHost("localhost") {
		t.Error("loopback, private and localhost are all local")
	}
	if isLocalHost("93.184.216.34") {
		t.Error("a public address is not local")
	}
	if !slicesContains([]string{"a", "b"}, "b") || slicesContains([]string{"a"}, "z") {
		t.Error("slicesContains is wrong")
	}
}

func TestRootIsWarnedAbout(t *testing.T) {
	original := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = original })

	c := find(t, Run(context.Background(), offlineOptions(baseConfig(t))), "privileges")
	if c.Status != Warn || !strings.Contains(c.Fix, "unprivileged") {
		t.Errorf("privileges = %+v, want a warning", c)
	}
}

func TestContainerDetectionUsesPlatformMarkers(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if !inContainer() {
		t.Error("a Kubernetes environment should be detected as a container")
	}
}

func TestUnreadableStorageTreeFails(t *testing.T) {
	requireStrictPermissions(t)
	cfg := baseConfig(t)
	blocked := filepath.Join(cfg.DataDir, "2026")
	if err := os.MkdirAll(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	c := find(t, Run(context.Background(), offlineOptions(cfg)), "data_dir")
	if c.Status != Fail || !strings.Contains(c.Fix, "chown") {
		t.Errorf("data_dir = %+v, want a failure with an ownership hint", c)
	}
}

func TestLongButGuessableTokensAreFlagged(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Tokens = []string{"this-is-my-secret-value-for-godrop"}
	c := find(t, Run(context.Background(), offlineOptions(cfg)), "token_strength")
	if c.Status != Fail {
		t.Errorf("token_strength = %+v, want a long-but-guessable token to be flagged", c)
	}
}

func TestTrustedCertificateStates(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsSrv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(tlsSrv.Certificate())

	cfg := baseConfig(t)
	cfg.BaseURL = tlsSrv.URL

	t.Run("healthy", func(t *testing.T) {
		report := Run(context.Background(), Options{
			Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1",
			TLSRoots: roots, Now: func() time.Time { return time.Now() },
		})
		if c := find(t, report, "tls"); c.Status != Pass {
			t.Errorf("tls = %+v, want pass for a trusted certificate", c)
		}
	})

	t.Run("close to expiry", func(t *testing.T) {
		report := Run(context.Background(), Options{
			Config: cfg, Runner: noFirewall, CheckURL: "http://127.0.0.1:1", TLSRoots: roots,
			Now: func() time.Time { return tlsSrv.Certificate().NotAfter.Add(-3 * 24 * time.Hour) },
		})
		c := find(t, report, "tls")
		if c.Status != Warn || !strings.Contains(c.Fix, "renewal") {
			t.Errorf("tls = %+v, want a renewal warning", c)
		}
	})
}

func TestEndToEndReportsANonOKDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"url": "http://" + r.Host + "/f/x"})
		case http.MethodGet:
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	report := Run(context.Background(), Options{
		TargetURL: srv.URL, Token: testToken, Offline: true, Runner: noFirewall,
	})
	c := find(t, report, "download")
	if c.Status != Fail || !strings.Contains(c.Detail, "403") {
		t.Errorf("download = %+v", c)
	}
}

// methodFailer makes specific HTTP methods fail at the transport level, which
// is how a connection dropped mid-diagnosis behaves.
type methodFailer struct {
	base   http.RoundTripper
	method string
}

func (t methodFailer) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == t.method {
		return nil, errors.New("connection reset by peer")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

func TestEndToEndReportsATransportFailureOnDownload(t *testing.T) {
	godrop, cfg := newGoDrop(t)
	report := Run(context.Background(), Options{
		Config: cfg, TargetURL: godrop.URL, Token: testToken, Offline: true, Runner: noFirewall,
		HTTP: &http.Client{Transport: methodFailer{godrop.Client().Transport, http.MethodGet}},
	})
	if c := find(t, report, "download"); c.Status != Fail {
		t.Errorf("download = %+v, want a failure", c)
	}
}

func TestEndToEndReportsATransportFailureOnDelete(t *testing.T) {
	godrop, cfg := newGoDrop(t)
	report := Run(context.Background(), Options{
		Config: cfg, TargetURL: godrop.URL, Token: testToken, Offline: true, Runner: noFirewall,
		HTTP: &http.Client{Transport: methodFailer{godrop.Client().Transport, http.MethodDelete}},
	})
	if c := find(t, report, "delete"); c.Status != Fail {
		t.Errorf("delete = %+v, want a failure", c)
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
