package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

// safeBuffer is a bytes.Buffer that may be read while a logging goroutine is
// still writing to it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freePort reserves and releases a port, so tests never collide with whatever
// else is running on the machine.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestServeRefusesToStartWithoutTokens(t *testing.T) {
	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "")

	code, _, stderr := run(t, testBuild(), "serve")
	if code == 0 {
		t.Fatal("serving without a token would accept anonymous uploads")
	}
	for _, want := range []string{"no API tokens", "godrop token create", "GODROP_TOKENS", "godrop init"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal should mention %q:\n%s", want, stderr)
		}
	}
}

func TestServeRejectsBrokenConfiguration(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_MAX_FILE_SIZE", "lots")

	code, _, stderr := run(t, testBuild(), "serve")
	if code == 0 {
		t.Fatal("a broken configuration should stop the server")
	}
	if !strings.Contains(stderr, "invalid configuration") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestServeReportsUnusableStorage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	t.Setenv("GODROP_DATA_DIR", filepath.Join(blocked, "data"))
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")

	if code, _, _ := run(t, testBuild(), "serve"); code == 0 {
		t.Fatal("an unusable data directory should stop the server")
	}
}

func TestServeReportsACorruptTokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(tokens.Path(dir), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "")

	if code, _, _ := run(t, testBuild(), "serve"); code == 0 {
		t.Fatal("a corrupt token file should stop the server")
	}
}

func TestServeReportsAnAddressAlreadyInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_ADDR", ln.Addr().String())
	t.Setenv("GODROP_LOG_LEVEL", "error")

	if code, _, stderr := run(t, testBuild(), "serve"); code == 0 {
		t.Fatalf("a busy port should be reported, stderr = %q", stderr)
	}
}

// serveInBackground starts the server and returns its address plus a stop
// function that waits for a clean shutdown.
func serveInBackground(t *testing.T, token string, env map[string]string) (string, func()) {
	t.Helper()
	addr := freePort(t)
	dir := t.TempDir()

	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", token)
	t.Setenv("GODROP_ADDR", addr)
	t.Setenv("GODROP_LOG_FORMAT", "text")
	for k, v := range env {
		t.Setenv(k, v)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- ExecuteWith(ctx, testBuild(), []string{"serve"}, io.Discard, io.Discard)
	}()

	// Wait for the listener rather than sleeping a fixed amount.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return "http://" + addr, func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("serve exited with %d", code)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve did not shut down when its context was cancelled")
		}
	}
}

func TestServeServesAndShutsDownCleanly(t *testing.T) {
	const token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	base, stop := serveInBackground(t, token, map[string]string{
		"GODROP_MAX_TOTAL_SIZE": "10MB",
		"GODROP_RETENTION":      "30d",
		"GODROP_RATE_LIMIT":     "100/m",
	})
	defer stop()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("the server is not answering: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}

	// An upload proves storage, tokens and routing are all wired together.
	body, contentType := smallUpload(t)
	req, err := http.NewRequest(http.MethodPost, base+"/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	up, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Body.Close()
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", up.StatusCode)
	}
	var got struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(up.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.URL, base+"/f/") {
		t.Errorf("url = %q", got.URL)
	}
}

func smallUpload(t *testing.T) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := newMultipart(&buf)
	return &buf, w
}

// newMultipart writes a one-file body and returns its content type.
func newMultipart(buf *bytes.Buffer) string {
	const boundary = "godroptestboundary"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"hello.txt\"\r\n\r\n")
	buf.WriteString("hello from the serve test\r\n")
	buf.WriteString("--" + boundary + "--\r\n")
	return "multipart/form-data; boundary=" + boundary
}

func TestServeWithTelemetryEnabled(t *testing.T) {
	// A telemetry endpoint that records the heartbeat the server sends at start.
	hits := make(chan struct{}, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	addr := freePort(t)
	dir := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_ADDR", addr)

	build := testBuild()
	build.PostHogKey = "phc_test"
	build.PostHogHost = "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ExecuteWith(ctx, build, []string{"serve"}, io.Discard, io.Discard) }()

	select {
	case <-hits:
	case <-time.After(10 * time.Second):
		t.Error("no heartbeat was sent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Error("the server did not shut down")
	}
}

func TestServeWithTelemetryDisabled(t *testing.T) {
	dir := t.TempDir()
	addr := freePort(t)
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_ADDR", addr)
	t.Setenv("GODROP_TELEMETRY", "off")

	build := testBuild()
	build.PostHogKey = "phc_test"
	build.PostHogHost = "http://127.0.0.1:1" // any request here would fail loudly

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ExecuteWith(ctx, build, []string{"serve"}, io.Discard, io.Discard) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit = %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Error("the server did not shut down")
	}
}

func TestCleanupRemovesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("txt", strings.NewReader("old"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file.Path, past, past); err != nil {
		t.Fatal(err)
	}

	var logs safeBuffer
	cfg := &config.Config{Retention: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runCleanup(ctx, store, cfg, slog.New(slog.NewTextHandler(&logs, nil)))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if files, _ := store.Stats(); files == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the expired file was not removed; logs:\n%s", logs.String())
}

func TestCleanupIsDisabledWithoutRetention(t *testing.T) {
	store, err := storage.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		runCleanup(context.Background(), store, &config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("without retention the cleaner should return immediately")
	}
}

func TestCleanupReportsFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
	dir := t.TempDir()
	store, err := storage.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("txt", strings.NewReader("old"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file.Path, past, past); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(file.Path)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	var logs safeBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runCleanup(ctx, store, &config.Config{Retention: time.Hour}, logger)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "cleanup failed") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a failing cleanup should be logged:\n%s", logs.String())
}

func TestFlushTokensStopsWithTheContext(t *testing.T) {
	store, err := tokens.New(tokens.Path(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		flushTokens(ctx, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the flusher should stop with its context")
	}
}

func TestLoggerFormats(t *testing.T) {
	if newLogger(&config.Config{LogFormat: "text"}) == nil {
		t.Error("a text logger should be created")
	}
	if newLogger(&config.Config{LogFormat: "json"}) == nil {
		t.Error("a JSON logger should be created")
	}
}
