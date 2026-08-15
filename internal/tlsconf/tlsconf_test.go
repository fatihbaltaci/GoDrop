package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// certificate writes a self-signed certificate and key, the way certbot or a
// company CA would leave them.
func certificate(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "files.example.com"},
		DNSNames:     []string{"files.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "fullchain.pem")
	keyFile = filepath.Join(dir, "privkey.pem")
	writePEM(t, certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certFile, keyFile
}

func writePEM(t *testing.T, path string, block *pem.Block) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, block); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTLSOffServesPlainHTTP(t *testing.T) {
	s, err := New(&config.Config{TLS: config.TLSOff})
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled() || s.Challenge != nil {
		t.Errorf("server = %+v, want nothing TLS about it", s)
	}
	if !strings.Contains(s.Describe, "plain http") {
		t.Errorf("Describe = %q", s.Describe)
	}
	var none *Server
	if none.Enabled() {
		t.Error("a nil Server is not enabled")
	}
}

func TestACertificateOnDiskIsUsedAsItIs(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := certificate(t, dir)
	s, err := New(&config.Config{TLS: config.TLSFile, TLSCert: certFile, TLSKey: keyFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.Enabled() || len(s.TLSConfig.Certificates) != 1 {
		t.Fatalf("server = %+v", s)
	}
	if s.CertFile != certFile || s.KeyFile != keyFile {
		t.Errorf("files = %q, %q", s.CertFile, s.KeyFile)
	}
	if s.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 or better", s.TLSConfig.MinVersion)
	}
	if !strings.Contains(s.Describe, certFile) {
		t.Errorf("Describe = %q, want it to name the certificate", s.Describe)
	}
}

func TestACertificateThatCannotBeReadStopsTheServer(t *testing.T) {
	// Better now, with a reason, than at the first request.
	dir := t.TempDir()
	broken := filepath.Join(dir, "not-a-cert.pem")
	if err := os.WriteFile(broken, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(&config.Config{TLS: config.TLSFile, TLSCert: broken, TLSKey: broken})
	if err == nil || !strings.Contains(err.Error(), "read the certificate") {
		t.Fatalf("err = %v", err)
	}
}

func TestAutomaticTLSIsSetUpForTheNamedDomains(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "acme")
	s, err := New(&config.Config{
		TLS:         config.TLSAuto,
		TLSDomains:  []string{"files.example.com", "cdn.example.com"},
		TLSCacheDir: cache,
		TLSEmail:    "ops@example.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.Enabled() || s.Challenge == nil {
		t.Fatalf("server = %+v", s)
	}
	if s.CertFile != "" || s.KeyFile != "" {
		t.Error("an automatic certificate does not come from files")
	}
	// The cache has to exist and be private: it holds the account key.
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatalf("the certificate cache should have been created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 && os.Getenv("GOOS") != "windows" {
		t.Logf("cache mode = %#o", perm)
	}
	// acme-tls/1 is what allows a certificate to be issued over 443 alone.
	var found bool
	for _, proto := range s.TLSConfig.NextProtos {
		if proto == acme.ALPNProto {
			found = true
		}
	}
	if !found {
		t.Errorf("NextProtos = %v, want it to offer %s", s.TLSConfig.NextProtos, acme.ALPNProto)
	}
	if !strings.Contains(s.Describe, "files.example.com") || !strings.Contains(s.Describe, "1 more") {
		t.Errorf("Describe = %q", s.Describe)
	}
}

func TestAutomaticTLSNeedsAUsableCache(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(&config.Config{
		TLS: config.TLSAuto, TLSDomains: []string{"x.example.com"},
		TLSCacheDir: filepath.Join(blocker, "acme"),
	})
	if err == nil || !strings.Contains(err.Error(), "certificate cache") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnUnknownModeIsRefused(t *testing.T) {
	if _, err := New(&config.Config{TLS: config.TLSMode("sometimes")}); err == nil {
		t.Fatal("an unknown mode should be reported rather than guessed at")
	}
}

func TestDescribeNamesTheSingleDomain(t *testing.T) {
	if got := join(nil); got != "no domain" {
		t.Errorf("join(nil) = %q", got)
	}
	if got := join([]string{"only.example.com"}); got != "only.example.com" {
		t.Errorf("join = %q", got)
	}
}

// ------------------------------------------------------------ the redirect

func TestPlainRequestsAreSentToHTTPS(t *testing.T) {
	cases := []struct {
		name, baseURL, host, path, want string
		hosts                           []string
	}{
		{
			name:    "the public URL knows the port",
			baseURL: "https://files.example.com:8443", host: "files.example.com",
			path: "/f/x.jpg", want: "https://files.example.com:8443/f/x.jpg",
		},
		{
			name:    "a trailing slash is not doubled",
			baseURL: "https://files.example.com/", host: "files.example.com",
			path: "/healthz", want: "https://files.example.com/healthz",
		},
		{
			name:    "without one, the port is dropped",
			baseURL: "", host: "files.example.com:80", hosts: []string{"files.example.com"},
			path: "/f/x.jpg", want: "https://files.example.com/f/x.jpg",
		},
		{
			name:    "the name is matched whatever its case",
			baseURL: "", host: "FILES.example.com", hosts: []string{"files.example.com"},
			path: "/healthz", want: "https://files.example.com/healthz",
		},
		{
			name:    "an IPv6 literal keeps its brackets",
			baseURL: "", host: "[2001:db8::1]", hosts: []string{"[2001:db8::1]"},
			path: "/healthz", want: "https://[2001:db8::1]/healthz",
		},
		{
			name:    "the query survives",
			baseURL: "", host: "files.example.com", hosts: []string{"files.example.com"},
			path: "/f/x.jpg?dl=1", want: "https://files.example.com/f/x.jpg?dl=1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			redirect(tc.baseURL, tc.hosts).ServeHTTP(rec, req)

			if rec.Code != http.StatusFound {
				t.Errorf("status = %d, want a temporary redirect", rec.Code)
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Errorf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------- the cache

func TestCheckCacheReportsWhatIsWrong(t *testing.T) {
	dir := t.TempDir()
	if err := CheckCache(dir); err != nil {
		t.Errorf("a writable directory is fine: %v", err)
	}
	if err := CheckCache(filepath.Join(dir, "not-there")); err == nil {
		t.Error("a missing cache should be reported")
	}

	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckCache(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckCacheReportsAnUnwritableDirectory(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := CheckCache(dir); err == nil {
		t.Error("a cache that cannot be written to is no use")
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

func TestARequestForSomebodyElsesNameIsNotRedirected(t *testing.T) {
	// Otherwise anyone could hand out a link that leaves through this server
	// and arrives somewhere else entirely.
	req := httptest.NewRequest(http.MethodGet, "/f/x.jpg", nil)
	req.Host = "evil.example.net"
	rec := httptest.NewRecorder()
	redirect("", []string{"files.example.com"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want the request refused", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Errorf("Location = %q, want none", got)
	}
}

func TestTheRedirectKnowsEveryNameTheServerAnswersFor(t *testing.T) {
	got := names(&config.Config{BaseURL: "https://files.example.com:8443"}, []string{"cdn.example.com"})
	want := []string{"cdn.example.com", "files.example.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("names = %v, want %v", got, want)
	}
	if got := names(&config.Config{}, nil); len(got) != 0 {
		t.Errorf("names = %v, want none", got)
	}
	if h := hostOf("://not a url"); h != "" {
		t.Errorf("hostOf = %q", h)
	}
}

func TestACertificateForAnAddressRedirectsToIt(t *testing.T) {
	// A certificate from a company CA is often issued for an address rather
	// than a name, and the redirect has to work there too.
	leaf := &x509.Certificate{
		DNSNames:    []string{"files.example.com"},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("2001:db8::1")},
	}
	got := certNames(leaf)
	want := []string{"files.example.com", "10.0.0.5", "[2001:db8::1]"}
	if len(got) != len(want) {
		t.Fatalf("certNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("certNames = %v, want %v", got, want)
		}
	}
}
