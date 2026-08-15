package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestNoKeyMeansNoTelemetry(t *testing.T) {
	// This is what makes "built from source never reports" true.
	c, err := New(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c != nil {
		t.Fatal("a build without a key must produce no client")
	}
	// A nil client is safe to use.
	if err := c.Send(context.Background()); err != nil {
		t.Errorf("Send on a nil client: %v", err)
	}
	c.Run(context.Background())
}

func TestPayloadContainsOnlyTheAgreedFields(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Options{
		Key: "phc_test", Version: "1.2.3", DataDir: dir,
		Env: envFrom(map[string]string{"FLY_APP_NAME": "godrop"}),
		Now: func() time.Time { return time.Date(2026, 8, 15, 14, 30, 22, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Payload()

	if p.Event != Event || p.APIKey != "phc_test" {
		t.Errorf("payload = %+v", p)
	}
	if p.DistinctID == "" {
		t.Error("an installation id is required to count installations")
	}
	if p.Timestamp != "2026-08-15T14:30:22Z" {
		t.Errorf("timestamp = %q", p.Timestamp)
	}

	want := map[string]any{
		"version": "1.2.3",
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
		"deploy":  "fly",
	}
	if len(p.Properties) != len(want) {
		t.Fatalf("properties = %v, want exactly %v", p.Properties, want)
	}
	for k, v := range want {
		if p.Properties[k] != v {
			t.Errorf("properties[%q] = %v, want %v", k, p.Properties[k], v)
		}
	}

	// Nothing about the files, the host or the network may appear.
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"file", "upload", "bytes", "url", "host", "ip", "token", dir} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Errorf("payload contains %q: %s", forbidden, encoded)
		}
	}
}

func TestSendPostsToTheCaptureEndpoint(t *testing.T) {
	var (
		mu     sync.Mutex
		gotURL string
		body   Payload
		agent  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotURL = r.URL.Path
		agent = r.Header.Get("User-Agent")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer srv.Close()

	c, err := New(Options{Key: "phc_test", Host: srv.URL + "/", Version: "9.9.9", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotURL != "/i/v0/e" {
		t.Errorf("path = %q, want the PostHog capture endpoint", gotURL)
	}
	if body.Event != Event {
		t.Errorf("event = %q", body.Event)
	}
	if !strings.HasPrefix(agent, "godrop/") {
		t.Errorf("User-Agent = %q", agent)
	}
}

func TestSendReportsServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := New(Options{Key: "k", Host: srv.URL, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background()); err == nil {
		t.Fatal("a failing endpoint should be reported to the caller")
	}
}

func TestSendReportsTransportErrors(t *testing.T) {
	c, err := New(Options{Key: "k", Host: "http://127.0.0.1:1", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background()); err == nil {
		t.Fatal("an unreachable endpoint should be reported")
	}
}

func TestSendRejectsAnInvalidHost(t *testing.T) {
	c, err := New(Options{Key: "k", Host: "http://\x7f", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background()); err == nil {
		t.Fatal("an unusable host should be reported")
	}
}

func TestRunSendsImmediatelyAndStopsWithTheContext(t *testing.T) {
	var calls sync.WaitGroup
	calls.Add(1)
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(calls.Done)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := New(Options{Key: "k", Host: srv.URL, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	calls.Wait() // the first heartbeat goes out without waiting for the ticker
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
}

func TestRunSurvivesAFailingEndpoint(t *testing.T) {
	c, err := New(Options{Key: "k", Host: "http://127.0.0.1:1", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a failing endpoint must not keep Run alive")
	}
}

func TestInstallIDIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := InstallID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Errorf("install id = %q, want 32 hex characters", first)
	}
	second, err := InstallID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("the installation id must survive a restart")
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("install id file mode = %#o, want 0600", perm)
	}
	// A different installation gets a different id.
	other, err := InstallID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two installations should not share an id")
	}
}

func TestInstallIDRegeneratesWhenBlank(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := InstallID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(id) == "" {
		t.Fatal("a blank file should produce a fresh id")
	}
}

func TestInstallIDReportsReadErrors(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("abc"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := InstallID(dir); err == nil {
		t.Fatal("an unreadable install id file should be reported")
	}
}

func TestInstallIDReportsWriteErrors(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := InstallID(dir); err == nil {
		t.Fatal("an unwritable data directory should be reported")
	}
}

func TestNewPropagatesInstallIDErrors(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := New(Options{Key: "k", DataDir: dir}); err == nil {
		t.Fatal("New should report that it cannot establish an installation id")
	}
}

func TestNewAcceptsAnInjectedInstallID(t *testing.T) {
	c, err := New(Options{Key: "k", InstallID: "fixed", DataDir: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	if c.Payload().DistinctID != "fixed" {
		t.Errorf("distinct id = %q", c.Payload().DistinctID)
	}
}

func TestDefaultHostIsTheEURegion(t *testing.T) {
	c, err := New(Options{Key: "k", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "https://eu.i.posthog.com" {
		t.Errorf("host = %q, want the EU region by default", c.host)
	}
}

func TestDetectDeploy(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"fly", map[string]string{"FLY_APP_NAME": "godrop"}, "fly"},
		{"render", map[string]string{"RENDER": "true"}, "render"},
		{"render by service id", map[string]string{"RENDER_SERVICE_ID": "srv-1"}, "render"},
		{"railway", map[string]string{"RAILWAY_ENVIRONMENT": "production"}, "railway"},
		{"railway by project", map[string]string{"RAILWAY_PROJECT_ID": "p-1"}, "railway"},
		{"kubernetes", map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "kubernetes"},
		{"systemd", map[string]string{"INVOCATION_ID": "abc"}, "systemd"},
		{"plain binary", nil, "binary"},
	}
	original := fileExists
	fileExists = func(string) bool { return false }
	t.Cleanup(func() { fileExists = original })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectDeploy(envFrom(tt.env)); got != tt.want {
				t.Errorf("DetectDeploy = %q, want %q", got, tt.want)
			}
		})
	}

	fileExists = func(path string) bool { return path == "/.dockerenv" }
	if got := DetectDeploy(envFrom(nil)); got != "docker" {
		t.Errorf("DetectDeploy inside a container = %q, want docker", got)
	}
}

func TestFileExistsChecksTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if fileExists(path) {
		t.Error("a missing file should not be reported as existing")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !fileExists(path) {
		t.Error("an existing file should be detected")
	}
}

func TestOptOutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if Disabled(dir) {
		t.Error("telemetry starts enabled")
	}
	if err := SetDisabled(dir, true); err != nil {
		t.Fatal(err)
	}
	if !Disabled(dir) {
		t.Error("SetDisabled(true) should be observable")
	}
	info, err := os.Stat(filepath.Join(dir, OptOutFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("opt-out file mode = %#o", perm)
	}
	if err := SetDisabled(dir, false); err != nil {
		t.Fatal(err)
	}
	if Disabled(dir) {
		t.Error("SetDisabled(false) should re-enable telemetry")
	}
	// Re-enabling when it was never disabled is not an error.
	if err := SetDisabled(dir, false); err != nil {
		t.Errorf("idempotent enable: %v", err)
	}
}

func TestSetDisabledReportsErrors(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := SetDisabled(dir, true); err == nil {
		t.Fatal("an unwritable directory should be reported")
	}

	blocked := filepath.Join(dir, "nested", "deeper")
	if err := SetDisabled(blocked, true); err == nil {
		t.Fatal("a directory that cannot be created should be reported")
	}
}

func TestSetDisabledReportsRemovalErrors(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	if err := SetDisabled(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := SetDisabled(dir, false); err == nil {
		t.Fatal("a failure to remove the opt-out marker should be reported")
	}
}

func TestClientUsesTheRealClockByDefault(t *testing.T) {
	c, err := New(Options{Key: "k", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := time.Parse(time.RFC3339, c.Payload().Timestamp)
	if err != nil {
		t.Fatalf("timestamp is not RFC3339: %v", err)
	}
	if time.Since(stamp) > time.Minute {
		t.Errorf("timestamp = %v, want the current time", stamp)
	}
}

func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}

func TestInstallIDReportsDirectoryCreationErrors(t *testing.T) {
	requireNonRoot(t)
	parent := t.TempDir()
	// Traversable but not writable: the lookup reports "not there yet", and
	// then creating the directory is refused.
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	if _, err := InstallID(filepath.Join(parent, "data")); err == nil {
		t.Fatal("a data directory that cannot be created should be reported")
	}
}
