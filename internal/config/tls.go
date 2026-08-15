package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// TLSMode says where the certificate comes from.
type TLSMode string

const (
	// TLSOff serves plain http. Right on loopback, a private network or
	// behind a proxy that terminates TLS itself.
	TLSOff TLSMode = "off"
	// TLSAuto obtains and renews a certificate from Let's Encrypt.
	TLSAuto TLSMode = "auto"
	// TLSFile uses a certificate and key already on disk, whether they came
	// from certbot, a company CA or a cloud provider.
	TLSFile TLSMode = "file"
)

// tlsCacheDir is where an automatic certificate is kept, inside the data
// directory so that it survives a restart and is backed up with everything
// else. Losing it means asking Let's Encrypt for a new certificate on every
// start, which runs into their rate limits.
const tlsCacheDir = "acme"

// loadTLS resolves the TLS settings and reports what is wrong with them.
//
// The mode can be left unset: a certificate and key make it "file", so the
// common case of "I already have a certificate" needs no extra variable.
func (cfg *Config) loadTLS(env Getenv, fail func(string, error)) {
	cfg.TLSCert = strings.TrimSpace(env("GODROP_TLS_CERT"))
	cfg.TLSKey = strings.TrimSpace(env("GODROP_TLS_KEY"))
	cfg.TLSEmail = strings.TrimSpace(env("GODROP_TLS_EMAIL"))
	cfg.TLSDomains = splitList(env("GODROP_TLS_DOMAINS"))

	mode, err := ParseTLSMode(env("GODROP_TLS"), cfg.TLSCert != "" || cfg.TLSKey != "")
	if err != nil {
		fail("GODROP_TLS", err)
		return
	}
	cfg.TLS = mode

	if cfg.TLS == TLSOff {
		return
	}

	// The ports change with TLS, unless they were set explicitly.
	if env("GODROP_ADDR") == "" {
		cfg.Addr = DefaultTLSAddr
	}
	cfg.HTTPAddr = str(env, "GODROP_HTTP_ADDR", DefaultHTTPAddr)
	if strings.EqualFold(cfg.HTTPAddr, "off") {
		cfg.HTTPAddr = ""
	}

	switch cfg.TLS {
	case TLSFile:
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			fail("GODROP_TLS_CERT", errors.New("both GODROP_TLS_CERT and GODROP_TLS_KEY are needed"))
		}
	case TLSAuto:
		cfg.TLSCacheDir = str(env, "GODROP_TLS_CACHE_DIR", filepath.Join(cfg.DataDir, tlsCacheDir))
		// A certificate is issued for a name, so there has to be one. The
		// public URL already carries it in the common case.
		if len(cfg.TLSDomains) == 0 {
			if host := hostOf(cfg.BaseURL); host != "" {
				cfg.TLSDomains = []string{host}
			}
		}
		if len(cfg.TLSDomains) == 0 {
			fail("GODROP_TLS_DOMAINS", errors.New(
				"automatic certificates need a domain: set GODROP_TLS_DOMAINS=files.example.com, or GODROP_BASE_URL"))
			return
		}
		for _, d := range cfg.TLSDomains {
			if err := ValidTLSDomain(d); err != nil {
				fail("GODROP_TLS_DOMAINS", err)
			}
		}
	case TLSOff:
	}
}

// ParseTLSMode reads the GODROP_TLS value. haveFiles reports whether a
// certificate was supplied, which selects file mode on its own.
func ParseTLSMode(value string, haveFiles bool) (TLSMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		if haveFiles {
			return TLSFile, nil
		}
		return TLSOff, nil
	case "off", "false", "no", "0", "none":
		return TLSOff, nil
	case "auto", "on", "true", "1", "letsencrypt", "acme":
		return TLSAuto, nil
	case "file", "files", "manual", "cert":
		return TLSFile, nil
	default:
		return TLSOff, fmt.Errorf("unknown value %q: use auto, file or off", value)
	}
}

// privateSuffixes never resolve on the public internet, so no public authority
// will ever issue for them. Tailscale names are in the list because a
// certificate for one comes from Tailscale itself, not from Let's Encrypt.
var privateSuffixes = []string{".local", ".internal", ".lan", ".home.arpa", ".ts.net", ".test", ".invalid", ".localhost"}

// ValidTLSDomain rejects the names Let's Encrypt will never issue for, with
// the reason, rather than letting the first certificate request fail at
// runtime and run into the rate limit for failed authorizations.
func ValidTLSDomain(d string) error {
	lower := strings.ToLower(d)
	switch {
	case d == "":
		return errors.New("empty domain")
	case net.ParseIP(d) != nil:
		return fmt.Errorf("%s is an address, and a public certificate can only be issued for a name", d)
	case strings.HasPrefix(d, "*"):
		return fmt.Errorf("%s is a wildcard, which needs a DNS challenge GoDrop does not do", d)
	case !strings.Contains(d, "."), lower == "localhost":
		return fmt.Errorf("%s is not a public domain name; use GODROP_TLS_CERT with your own certificate", d)
	}
	for _, suffix := range privateSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return fmt.Errorf("%s is a private name that no public authority can issue for; "+
				"use GODROP_TLS_CERT with your own certificate, or leave TLS off on a network you trust", d)
		}
	}
	return nil
}

// hostOf returns the host of a base URL, without its port.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// splitList parses a comma separated list, ignoring empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
