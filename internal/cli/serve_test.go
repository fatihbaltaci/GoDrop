package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
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
	requireStrictPermissions(t)
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

	waitForListener(t, addr)

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

func TestServeReportsATokenFileItCanNoLongerRead(t *testing.T) {
	// Reloading fails open, so a broken token file leaves the server running
	// with the last good copy, and a revoked token still valid. The operator
	// only finds out if it says so.
	const token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	addr := freePort(t)
	dir := t.TempDir()
	t.Setenv("GODROP_DATA_DIR", dir)
	t.Setenv("GODROP_TOKENS", token)
	t.Setenv("GODROP_ADDR", addr)
	t.Setenv("GODROP_LOG_FORMAT", "text")

	logs := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ExecuteWith(ctx, testBuild(), []string{"serve"}, logs, io.Discard) }()
	waitForListener(t, addr)
	defer func() {
		cancel()
		<-done
	}()

	if err := os.WriteFile(tokens.Path(dir), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Authentication only re-reads the file once the reload throttle expires.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/stats", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("the environment token should still work, got %d", resp.StatusCode)
			}
		}
		if strings.Contains(logs.String(), "token file could not be reloaded") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the broken token file was never reported:\n%s", logs.String())
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
	requireStrictPermissions(t)
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
	if newLogger(&config.Config{LogFormat: "text"}, io.Discard) == nil {
		t.Error("a text logger should be created")
	}
	if newLogger(&config.Config{LogFormat: "json"}, io.Discard) == nil {
		t.Error("a JSON logger should be created")
	}
}

// ------------------------------------------------------------------- HTTPS

// selfSignedCert writes a certificate and key, standing in for whatever
// certbot or a company CA would have left on disk.
func selfSignedCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "fullchain.pem")
	keyFile = filepath.Join(dir, "privkey.pem")
	write := func(path string, block *pem.Block) {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	write(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certFile, keyFile
}

func TestServeOverHTTPSWithYourOwnCertificate(t *testing.T) {
	const token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	certDir := t.TempDir()
	certFile, keyFile := selfSignedCert(t, certDir)
	plain := freePort(t)

	base, stop := serveInBackground(t, token, map[string]string{
		"GODROP_TLS_CERT":  certFile,
		"GODROP_TLS_KEY":   keyFile,
		"GODROP_HTTP_ADDR": plain,
	})
	defer stop()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a self-signed certificate made by this test
	}}
	httpsURL := "https://" + strings.TrimPrefix(base, "http://")
	resp, err := client.Get(httpsURL + "/healthz")
	if err != nil {
		t.Fatalf("the server is not answering over https: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}

	// Port 80 exists only to send people to https.
	waitForListener(t, plain)
	noRedirects := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	moved, err := noRedirects.Get("http://" + plain + "/f/x.jpg")
	if err != nil {
		t.Fatalf("the plain listener is not answering: %v", err)
	}
	defer moved.Body.Close()
	if moved.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want a redirect to https", moved.StatusCode)
	}
	if got := moved.Header.Get("Location"); !strings.HasPrefix(got, "https://") {
		t.Errorf("Location = %q", got)
	}
}

func TestServeStopsWhenTheCertificateCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "fullchain.pem")
	if err := os.WriteFile(broken, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90")
	t.Setenv("GODROP_ADDR", freePort(t))
	t.Setenv("GODROP_TLS_CERT", broken)
	t.Setenv("GODROP_TLS_KEY", broken)

	var out bytes.Buffer
	if code := ExecuteWith(context.Background(), testBuild(), []string{"serve"}, io.Discard, &out); code == 0 {
		t.Fatal("serve should refuse to start with a certificate it cannot read")
	}
	if !strings.Contains(out.String(), "read the certificate") {
		t.Errorf("output = %q, want it to say what is wrong", out.String())
	}
}

func TestTLSDescription(t *testing.T) {
	cases := []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{}, "off"},
		{config.Config{TLS: config.TLSFile, TLSCert: "/etc/ssl/fullchain.pem"}, "/etc/ssl/fullchain.pem"},
		{config.Config{TLS: config.TLSAuto, TLSDomains: []string{"a.example.com", "b.example.com"}},
			"automatic (a.example.com, b.example.com)"},
	}
	for _, tc := range cases {
		if got := tlsDescription(&tc.cfg); got != tc.want {
			t.Errorf("tlsDescription(%q) = %q, want %q", tc.cfg.TLS, got, tc.want)
		}
	}
}

func TestServeKeepsRunningWhenPort80IsTaken(t *testing.T) {
	// Losing the redirect listener is worth a warning, not a shutdown: https
	// still works, and the certificate can still be renewed over 443.
	const token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	certFile, keyFile := selfSignedCert(t, t.TempDir())
	addr := freePort(t)

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", token)
	t.Setenv("GODROP_ADDR", addr)
	t.Setenv("GODROP_LOG_FORMAT", "text")
	t.Setenv("GODROP_TLS_CERT", certFile)
	t.Setenv("GODROP_TLS_KEY", keyFile)
	t.Setenv("GODROP_HTTP_ADDR", taken.Addr().String())

	logs := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- ExecuteWith(ctx, testBuild(), []string{"serve"}, logs, io.Discard) }()
	defer func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("serve exited with %d", code)
		}
	}()
	waitForListener(t, addr)

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "the http listener stopped") {
		if time.Now().After(deadline) {
			t.Fatalf("the failed listener was never reported:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
