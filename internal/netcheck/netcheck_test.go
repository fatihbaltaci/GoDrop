package netcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublicIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fl=abc\nh=example.com\nip=203.0.113.9\nts=1\n")
	}))
	defer srv.Close()

	// The production endpoint is fixed, so point the client at the fake one.
	client := srv.Client()
	client.Transport = rewriteHost(srv.URL)

	ip, err := PublicIP(context.Background(), client)
	if err != nil {
		t.Fatalf("PublicIP: %v", err)
	}
	if ip != "203.0.113.9" {
		t.Errorf("ip = %q", ip)
	}
}

func TestPublicIPWithoutAnIPLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fl=abc\nh=example.com\n")
	}))
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHost(srv.URL)

	if _, err := PublicIP(context.Background(), client); err == nil {
		t.Fatal("a response without an ip line should be an error")
	}
}

func TestPublicIPTransportError(t *testing.T) {
	client := &http.Client{Transport: failingTransport{}}
	if _, err := PublicIP(context.Background(), client); err == nil {
		t.Fatal("a transport failure should be reported")
	}
}

func TestPublicIPRejectsABadContext(t *testing.T) {
	//lint:ignore SA1012 a nil context is exactly what we are testing
	if _, err := PublicIP(nil, http.DefaultClient); err == nil { //nolint:staticcheck
		t.Fatal("an unusable context should be reported")
	}
}

func TestResolveLocalhost(t *testing.T) {
	addrs, err := Resolve(context.Background(), nil, "localhost")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("localhost should resolve to at least one address")
	}
	if _, err := Resolve(context.Background(), net.DefaultResolver, "invalid.invalid."); err == nil {
		t.Error("a reserved invalid name should not resolve")
	}
}

func TestCheckTLSWithATrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "https://")

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	info := CheckTLS(context.Background(), hostport, time.Now(), roots)
	if !info.Attempted {
		t.Error("the check should record that it ran")
	}
	if !info.Valid {
		t.Fatalf("a trusted certificate should validate, got %q", info.Error)
	}
	if info.DaysLeft <= 0 {
		t.Errorf("DaysLeft = %d, want the remaining lifetime", info.DaysLeft)
	}
	if info.NotAfter.IsZero() {
		t.Error("the expiry date should be reported")
	}
}

func TestCheckTLSDescribesAnUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "https://")

	// No roots: the certificate is untrusted, but its details must still be
	// reported so the operator learns something useful.
	info := CheckTLS(context.Background(), hostport, time.Now(), x509.NewCertPool())
	if info.Valid {
		t.Error("an untrusted certificate must not validate")
	}
	if info.Error == "" {
		t.Error("the reason should be reported")
	}
	if info.NotAfter.IsZero() {
		t.Error("the expiry date should be reported even for an untrusted certificate")
	}
}

func TestCheckTLSExpiredCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "https://")
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	future := srv.Certificate().NotAfter.Add(24 * time.Hour)
	info := CheckTLS(context.Background(), hostport, future, roots)
	if info.Valid {
		t.Error("an expired certificate must not validate")
	}
	if info.DaysLeft >= 0 {
		t.Errorf("DaysLeft = %d, want a negative value once expired", info.DaysLeft)
	}
}

func TestCheckTLSUnreachableHost(t *testing.T) {
	info := CheckTLS(context.Background(), "127.0.0.1:1", time.Now(), nil)
	if info.Error == "" {
		t.Error("an unreachable host should be reported")
	}
	if info.Valid {
		t.Error("a failed dial must not report a valid certificate")
	}
}

func TestCheckTLSMalformedAddress(t *testing.T) {
	info := CheckTLS(context.Background(), "no-port-here", time.Now(), nil)
	if info.Error == "" {
		t.Error("an address without a port should be reported")
	}
}

func TestExternalReachability(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(ExternalResult{OK: true, Status: 200, Location: "FRA"})
	}))
	defer srv.Close()

	res, err := External(context.Background(), srv.Client(), srv.URL, "https://files.example.com/healthz")
	if err != nil {
		t.Fatalf("External: %v", err)
	}
	if !res.OK || res.Status != 200 || res.Location != "FRA" {
		t.Errorf("result = %+v", res)
	}
	if got["url"] != "https://files.example.com/healthz" {
		t.Errorf("the probe was asked to fetch %q", got["url"])
	}
	if len(got) != 1 {
		t.Errorf("only the URL may be sent, got %v", got)
	}
}

func TestExternalErrors(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		if _, err := External(context.Background(), srv.Client(), srv.URL, "https://x"); err == nil {
			t.Error("an error status should be reported")
		}
	})
	t.Run("garbage body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "not json")
		}))
		defer srv.Close()
		if _, err := External(context.Background(), srv.Client(), srv.URL, "https://x"); err == nil {
			t.Error("an unparsable body should be reported")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		if _, err := External(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "https://x"); err == nil {
			t.Error("an unreachable probe should be reported")
		}
	})
	t.Run("bad url", func(t *testing.T) {
		if _, err := External(context.Background(), http.DefaultClient, "http://\x7f", "https://x"); err == nil {
			t.Error("an unusable probe URL should be reported")
		}
	})
	t.Run("default endpoint is used", func(t *testing.T) {
		// No network here: the point is that an empty checkURL is filled in.
		_, err := External(context.Background(), &http.Client{Transport: failingTransport{}}, "", "https://x")
		if err == nil {
			t.Error("expected the transport error from the default endpoint")
		}
	})
}

func TestCheckFirewall(t *testing.T) {
	tests := []struct {
		name     string
		run      Runner
		wantTool string
		wantOpen bool
		wantHint bool
	}{
		{
			name: "ufw allows the port",
			run: fakeRunner(map[string]string{
				"ufw": "Status: active\n443/tcp                     ALLOW       Anywhere\n",
			}),
			wantTool: "ufw", wantOpen: true,
		},
		{
			name: "ufw blocks the port",
			run: fakeRunner(map[string]string{
				"ufw": "Status: active\n22/tcp                      ALLOW       Anywhere\n",
			}),
			wantTool: "ufw", wantOpen: false, wantHint: true,
		},
		{
			name:     "ufw inactive means nothing is blocked",
			run:      fakeRunner(map[string]string{"ufw": "Status: inactive\n"}),
			wantTool: "ufw", wantOpen: true,
		},
		{
			name: "firewalld allows the port",
			run: fakeRunner(map[string]string{
				"firewall-cmd": "public\n  ports: 443/tcp\n",
			}),
			wantTool: "firewalld", wantOpen: true,
		},
		{
			name:     "firewalld blocks the port",
			run:      fakeRunner(map[string]string{"firewall-cmd": "public\n  ports:\n"}),
			wantTool: "firewalld", wantOpen: false, wantHint: true,
		},
		{
			name:     "nftables with a matching rule",
			run:      fakeRunner(map[string]string{"nft": "table inet filter {\n tcp dport 443 accept\n}"}),
			wantTool: "nftables", wantOpen: true,
		},
		{
			name:     "nftables without a matching rule",
			run:      fakeRunner(map[string]string{"nft": "table inet filter {\n tcp dport 22 accept\n}"}),
			wantTool: "nftables", wantOpen: false, wantHint: true,
		},
		{
			name:     "empty nftables ruleset",
			run:      fakeRunner(map[string]string{"nft": "  "}),
			wantTool: "nftables", wantOpen: true,
		},
		{
			name:     "no firewall tooling at all",
			run:      fakeRunner(nil),
			wantTool: "", wantOpen: false, wantHint: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := CheckFirewall(context.Background(), tt.run, 443)
			if fw.Tool != tt.wantTool {
				t.Errorf("tool = %q, want %q", fw.Tool, tt.wantTool)
			}
			if fw.PortOpen != tt.wantOpen {
				t.Errorf("PortOpen = %t, want %t", fw.PortOpen, tt.wantOpen)
			}
			if (fw.Hint != "") != tt.wantHint {
				t.Errorf("hint = %q, want a hint: %t", fw.Hint, tt.wantHint)
			}
		})
	}
}

func TestCheckFirewallHttpServiceCountsForWebPorts(t *testing.T) {
	run := fakeRunner(map[string]string{"firewall-cmd": "public\n  services: ssh http https\n"})
	fw := CheckFirewall(context.Background(), run, 80)
	if !fw.PortOpen {
		t.Error("an http service entry should count as port 80 being open")
	}
}

func TestCheckFirewallUsesTheRealRunnerByDefault(t *testing.T) {
	// Whatever this machine has, the call must not panic and must return a
	// usable report.
	fw := CheckFirewall(context.Background(), nil, 443)
	if fw.Tool == "" && fw.Hint == "" {
		t.Error("an uninspectable firewall should still produce guidance")
	}
}

func TestExecRunner(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("ExecRunner: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q", out)
	}
	if _, err := ExecRunner(context.Background(), "godrop-no-such-command-12345"); err == nil {
		t.Error("a missing command should be reported")
	}
	if _, err := ExecRunner(context.Background(), "false"); err == nil {
		t.Error("a non-zero exit should be reported")
	}
}

func TestListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if !Listening(context.Background(), ln.Addr().String()) {
		t.Error("an open listener should be detected")
	}
	if Listening(context.Background(), "127.0.0.1:1") {
		t.Error("a closed port should not be reported as listening")
	}
}

func TestHostPort(t *testing.T) {
	tests := map[string]string{
		"https://files.example.com":      "files.example.com:443",
		"http://files.example.com":       "files.example.com:80",
		"https://files.example.com:8443": "files.example.com:8443",
		"http://127.0.0.1:8080":          "127.0.0.1:8080",
	}
	for in, want := range tests {
		got, err := HostPort(in)
		if err != nil {
			t.Errorf("HostPort(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("HostPort(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "not a url", "://x", "http://\x7f"} {
		if _, err := HostPort(bad); err == nil {
			t.Errorf("HostPort(%q) should fail", bad)
		}
	}
}

func TestPublicIPReportsBodyReadErrors(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(errorReader{}),
			Header:     make(http.Header),
		}, nil
	})}
	if _, err := PublicIP(context.Background(), client); err == nil {
		t.Fatal("a truncated response should be reported")
	}
}

func TestExternalReportsBodyReadErrors(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(errorReader{}),
			Header:     make(http.Header),
		}, nil
	})}
	if _, err := External(context.Background(), client, "http://probe.invalid", "https://x"); err == nil {
		t.Fatal("a truncated response should be reported")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestFirstLine(t *testing.T) {
	if got := firstLine("  one\ntwo\n"); got != "one" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine of an empty string = %q", got)
	}
}

// ------------------------------------------------------------------ helpers

// rewriteHost sends every request to the test server, so code with a hard-coded
// endpoint can be exercised without touching the network.
func rewriteHost(base string) http.RoundTripper {
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

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network is down")
}

// fakeRunner reports success only for the commands present in the map, which is
// how a machine with just one firewall tool installed behaves.
func fakeRunner(outputs map[string]string) Runner {
	return func(_ context.Context, name string, _ ...string) (string, error) {
		if out, ok := outputs[name]; ok {
			return out, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
}

func TestInspectCertsWithoutACertificate(t *testing.T) {
	info := inspectCerts(nil, "files.example.com", time.Now(), nil)
	if info.Error == "" || info.Valid {
		t.Errorf("info = %+v, want a reported failure", info)
	}
}

func TestInspectCertsValidatesAFullChain(t *testing.T) {
	// A realistic deployment presents leaf + intermediate, with only the root
	// in the trust store.
	root, rootKey := makeCert(t, "GoDrop Test Root", nil, nil, true, time.Now().Add(time.Hour))
	inter, interKey := makeCert(t, "GoDrop Test Intermediate", root, rootKey, true, time.Now().Add(time.Hour))
	leaf, _ := makeCert(t, "files.example.com", inter, interKey, false, time.Now().Add(48*time.Hour))

	roots := x509.NewCertPool()
	roots.AddCert(root)

	info := inspectCerts([]*x509.Certificate{leaf, inter}, "files.example.com", time.Now(), roots)
	if !info.Valid {
		t.Fatalf("a complete chain should validate, got %q", info.Error)
	}
	if info.Issuer != "GoDrop Test Intermediate" {
		t.Errorf("issuer = %q", info.Issuer)
	}
	if info.DaysLeft != 1 {
		t.Errorf("DaysLeft = %d, want 1", info.DaysLeft)
	}

	// Without the intermediate the chain cannot be built — the classic
	// "works in curl, fails in a browser" misconfiguration.
	if got := inspectCerts([]*x509.Certificate{leaf}, "files.example.com", time.Now(), roots); got.Valid {
		t.Error("an incomplete chain must not validate")
	}
}

// makeCert issues a certificate signed by parent, or self-signed when parent is
// nil, so chain handling can be tested without touching the network.
func makeCert(t *testing.T, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.BasicConstraintsValid = true
	} else {
		tmpl.DNSNames = []string{name}
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}
