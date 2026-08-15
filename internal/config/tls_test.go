package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// envOf builds a fake environment, so the tests never touch the real one.
func envOf(pairs map[string]string) Getenv {
	return func(key string) string { return pairs[key] }
}

// A token is always required, so every case supplies one.
func withToken(pairs map[string]string) map[string]string {
	if pairs == nil {
		pairs = map[string]string{}
	}
	pairs["GODROP_TOKENS"] = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	return pairs
}

func TestTLSIsOffUnlessAskedFor(t *testing.T) {
	cfg, err := LoadFrom(envOf(withToken(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS != TLSOff {
		t.Errorf("TLS = %q, want it off by default", cfg.TLS)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.HTTPAddr != "" {
		t.Errorf("HTTPAddr = %q, want nothing listening on 80", cfg.HTTPAddr)
	}
}

func TestACertificateOnItsOwnSelectsFileMode(t *testing.T) {
	// The common case, "I already have a certificate", needs no extra
	// variable to say so.
	cfg, err := LoadFrom(envOf(withToken(map[string]string{
		"GODROP_TLS_CERT": "/etc/ssl/fullchain.pem",
		"GODROP_TLS_KEY":  "/etc/ssl/privkey.pem",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS != TLSFile {
		t.Errorf("TLS = %q, want file", cfg.TLS)
	}
	if cfg.Addr != DefaultTLSAddr || cfg.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("addresses = %q and %q, want the ports a browser expects", cfg.Addr, cfg.HTTPAddr)
	}
}

func TestHalfACertificateIsReported(t *testing.T) {
	_, err := LoadFrom(envOf(withToken(map[string]string{"GODROP_TLS_CERT": "/etc/ssl/fullchain.pem"})))
	if err == nil || !strings.Contains(err.Error(), "GODROP_TLS_KEY") {
		t.Fatalf("err = %v, want it to say the key is missing too", err)
	}
}

func TestAutomaticTLSTakesItsDomainFromTheBaseURL(t *testing.T) {
	cfg, err := LoadFrom(envOf(withToken(map[string]string{
		"GODROP_TLS":      "auto",
		"GODROP_BASE_URL": "https://files.example.com",
		"GODROP_DATA_DIR": "/var/lib/godrop",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TLSDomains) != 1 || cfg.TLSDomains[0] != "files.example.com" {
		t.Errorf("domains = %v, want the host from the base URL", cfg.TLSDomains)
	}
	if want := filepath.Join("/var/lib/godrop", "acme"); cfg.TLSCacheDir != want {
		t.Errorf("cache = %q, want %q", cfg.TLSCacheDir, want)
	}
}

func TestAutomaticTLSNeedsADomain(t *testing.T) {
	_, err := LoadFrom(envOf(withToken(map[string]string{"GODROP_TLS": "auto"})))
	if err == nil || !strings.Contains(err.Error(), "GODROP_TLS_DOMAINS") {
		t.Fatalf("err = %v, want it to ask for a domain", err)
	}
}

func TestAutomaticTLSRefusesNamesLetsEncryptWillNotIssueFor(t *testing.T) {
	// Saying so now is much better than a certificate request failing in a
	// loop after the service is already running.
	cases := map[string]string{
		"192.0.2.10":           "address",
		"localhost":            "public domain name",
		"godrop":               "public domain name",
		"*.example.com":        "wildcard",
		"nas.local":            "private name",
		"files.internal":       "private name",
		"laptop.tail1a.ts.net": "private name",
		"box.HOME.ARPA":        "private name",
	}
	for domain, want := range cases {
		_, err := LoadFrom(envOf(withToken(map[string]string{
			"GODROP_TLS": "auto", "GODROP_TLS_DOMAINS": domain,
		})))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", domain, err, want)
		}
	}
	// An empty entry cannot arrive through the list parser, so the check is
	// exercised directly.
	if err := ValidTLSDomain(""); err == nil {
		t.Error("an empty domain should be refused")
	}
}

func TestSeveralDomainsAreAccepted(t *testing.T) {
	cfg, err := LoadFrom(envOf(withToken(map[string]string{
		"GODROP_TLS":         "auto",
		"GODROP_TLS_DOMAINS": "files.example.com, cdn.example.com ,",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TLSDomains) != 2 {
		t.Errorf("domains = %v, want the two names and nothing empty", cfg.TLSDomains)
	}
}

func TestThePortsCanStillBeChosen(t *testing.T) {
	cfg, err := LoadFrom(envOf(withToken(map[string]string{
		"GODROP_TLS":         "auto",
		"GODROP_TLS_DOMAINS": "files.example.com",
		"GODROP_ADDR":        ":8443",
		"GODROP_HTTP_ADDR":   "off",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8443" {
		t.Errorf("Addr = %q, want the explicit one to win", cfg.Addr)
	}
	if cfg.HTTPAddr != "" {
		t.Errorf("HTTPAddr = %q, want nothing on 80: autocert can still answer over 443", cfg.HTTPAddr)
	}
}

func TestParseTLSMode(t *testing.T) {
	cases := map[string]TLSMode{
		"":            TLSOff,
		"off":         TLSOff,
		"no":          TLSOff,
		"auto":        TLSAuto,
		"on":          TLSAuto,
		"letsencrypt": TLSAuto,
		"AUTO":        TLSAuto,
		"file":        TLSFile,
		"manual":      TLSFile,
	}
	for value, want := range cases {
		got, err := ParseTLSMode(value, false)
		if err != nil || got != want {
			t.Errorf("ParseTLSMode(%q) = %q, %v, want %q", value, got, err, want)
		}
	}
	if got, _ := ParseTLSMode("", true); got != TLSFile {
		t.Errorf("with a certificate supplied and no mode, want file, got %q", got)
	}
	if _, err := ParseTLSMode("sometimes", false); err == nil {
		t.Error("an unknown value should be reported")
	}
}

func TestAnUnknownTLSValueIsReportedWithEverythingElse(t *testing.T) {
	_, err := LoadFrom(envOf(withToken(map[string]string{
		"GODROP_TLS":           "yes-please",
		"GODROP_MAX_FILE_SIZE": "lots",
	})))
	if err == nil || !strings.Contains(err.Error(), "GODROP_TLS:") ||
		!strings.Contains(err.Error(), "GODROP_MAX_FILE_SIZE") {
		t.Fatalf("err = %v, want both problems reported at once", err)
	}
}

func TestHostOfIgnoresRubbish(t *testing.T) {
	if got := hostOf("://not a url"); got != "" {
		t.Errorf("hostOf = %q, want nothing", got)
	}
}
