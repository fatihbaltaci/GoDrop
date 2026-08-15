// Package config loads and validates GoDrop's runtime configuration from the
// environment. Every knob has a safe default except the token list, which must
// be set explicitly: a file upload service that accepts anonymous writes is a
// bug, not a convenience.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	// 8080 is the busiest port on any developer machine, and a service that
	// refuses to start because something else got there first is a bad first
	// impression. This one is unlikely to be taken and is outside the range
	// Linux hands out for outgoing connections.
	DefaultAddr = ":8747"
	// With TLS on, the ports are the ones a browser expects: 443 serves, and
	// 80 answers the certificate challenge and redirects.
	DefaultTLSAddr  = ":443"
	DefaultHTTPAddr = ":80"

	DefaultDataDir            = "./data"
	DefaultMaxFileSize        = 100 << 20 // 100MB
	DefaultMaxFilesPerRequest = 20
	DefaultReadHeaderTimeout  = 10 * time.Second
	DefaultIdleTimeout        = 120 * time.Second
	DefaultShutdownTimeout    = 30 * time.Second
	DefaultCORSOrigins        = "*"
	DefaultLogFormat          = "json"
	DefaultLogLevel           = "info"
)

// Config is the fully resolved configuration. Zero values are meaningful:
// MaxTotalSize 0 means "no quota", Retention 0 means "keep forever", and a nil
// RateLimit means "unlimited".
type Config struct {
	Addr    string
	DataDir string
	BaseURL string
	Tokens  []string

	// TLS. Mode is off, auto or file; see ParseTLSMode.
	TLS         TLSMode
	TLSDomains  []string
	TLSCert     string
	TLSKey      string
	TLSEmail    string
	TLSCacheDir string
	// HTTPAddr serves the ACME challenge and redirects to https. Empty
	// disables it, which leaves autocert to answer over TLS-ALPN on 443.
	HTTPAddr string

	MaxFileSize        int64
	MaxFilesPerRequest int
	MaxTotalSize       int64
	Retention          time.Duration

	RateLimit     *Rate
	AuthRateLimit *Rate

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	CORSOrigins []string

	LogFormat string
	LogLevel  slog.Level
	AccessLog bool

	Telemetry bool
}

// Rate is a request allowance expressed as N requests per Period.
type Rate struct {
	N      int
	Period time.Duration
}

func (r Rate) String() string {
	switch r.Period {
	case time.Second:
		return fmt.Sprintf("%d/s", r.N)
	case time.Minute:
		return fmt.Sprintf("%d/m", r.N)
	case time.Hour:
		return fmt.Sprintf("%d/h", r.N)
	default:
		return fmt.Sprintf("%d/%s", r.N, r.Period)
	}
}

// Getenv looks up an environment variable. It exists so tests (and the wizard)
// can supply a fake environment without mutating the real process state.
type Getenv func(string) string

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration using the supplied lookup function.
func LoadFrom(env Getenv) (*Config, error) {
	cfg := &Config{
		Addr:              str(env, "GODROP_ADDR", DefaultAddr),
		DataDir:           str(env, "GODROP_DATA_DIR", DefaultDataDir),
		BaseURL:           strings.TrimRight(env("GODROP_BASE_URL"), "/"),
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		ShutdownTimeout:   DefaultShutdownTimeout,
		AccessLog:         true,
		Telemetry:         true,
	}

	var errs []error
	fail := func(key string, err error) { errs = append(errs, fmt.Errorf("%s: %w", key, err)) }

	// Tokens may also come from the token file managed by `godrop token`, so an
	// empty list is not an error here. The server refuses to start when both
	// sources are empty. See cli.runServe.
	cfg.Tokens = ParseTokens(env("GODROP_TOKENS"))

	var err error
	if cfg.MaxFileSize, err = sizeOr(env, "GODROP_MAX_FILE_SIZE", DefaultMaxFileSize); err != nil {
		fail("GODROP_MAX_FILE_SIZE", err)
	} else if cfg.MaxFileSize <= 0 {
		fail("GODROP_MAX_FILE_SIZE", errors.New("must be greater than zero"))
	}
	if cfg.MaxTotalSize, err = sizeOr(env, "GODROP_MAX_TOTAL_SIZE", 0); err != nil {
		fail("GODROP_MAX_TOTAL_SIZE", err)
	}
	if cfg.MaxFilesPerRequest, err = intOr(env, "GODROP_MAX_FILES_PER_REQUEST", DefaultMaxFilesPerRequest); err != nil {
		fail("GODROP_MAX_FILES_PER_REQUEST", err)
	} else if cfg.MaxFilesPerRequest <= 0 {
		fail("GODROP_MAX_FILES_PER_REQUEST", errors.New("must be greater than zero"))
	}
	if cfg.Retention, err = durationOr(env, "GODROP_RETENTION", 0); err != nil {
		fail("GODROP_RETENTION", err)
	} else if cfg.Retention < 0 {
		fail("GODROP_RETENTION", errors.New("must not be negative"))
	}

	if cfg.RateLimit, err = rateOr(env, "GODROP_RATE_LIMIT"); err != nil {
		fail("GODROP_RATE_LIMIT", err)
	}
	if cfg.AuthRateLimit, err = rateOr(env, "GODROP_AUTH_RATE_LIMIT"); err != nil {
		fail("GODROP_AUTH_RATE_LIMIT", err)
	}

	for _, t := range []struct {
		key string
		dst *time.Duration
		def time.Duration
	}{
		{"GODROP_READ_HEADER_TIMEOUT", &cfg.ReadHeaderTimeout, DefaultReadHeaderTimeout},
		{"GODROP_READ_TIMEOUT", &cfg.ReadTimeout, 0},
		{"GODROP_WRITE_TIMEOUT", &cfg.WriteTimeout, 0},
		{"GODROP_IDLE_TIMEOUT", &cfg.IdleTimeout, DefaultIdleTimeout},
		{"GODROP_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout, DefaultShutdownTimeout},
	} {
		v, err := durationOr(env, t.key, t.def)
		if err != nil {
			fail(t.key, err)
			continue
		}
		if v < 0 {
			fail(t.key, errors.New("must not be negative"))
			continue
		}
		*t.dst = v
	}

	cfg.CORSOrigins = ParseList(str(env, "GODROP_CORS_ORIGINS", DefaultCORSOrigins))

	cfg.loadTLS(env, fail)

	cfg.LogFormat = strings.ToLower(str(env, "GODROP_LOG_FORMAT", DefaultLogFormat))
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		fail("GODROP_LOG_FORMAT", errors.New(`must be "json" or "text"`))
	}
	if cfg.LogLevel, err = parseLevel(str(env, "GODROP_LOG_LEVEL", DefaultLogLevel)); err != nil {
		fail("GODROP_LOG_LEVEL", err)
	}
	if cfg.AccessLog, err = boolOr(env, "GODROP_ACCESS_LOG", true); err != nil {
		fail("GODROP_ACCESS_LOG", err)
	}
	if cfg.Telemetry, err = telemetryOr(env); err != nil {
		fail("GODROP_TELEMETRY", err)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// PublicURL returns the absolute URL for a stored file path such as
// "/f/<id>/<name>". When BaseURL is empty the caller-supplied scheme and host
// (derived from the request) are used instead.
func (c *Config) PublicURL(scheme, host, path string) string {
	if c.BaseURL != "" {
		return c.BaseURL + path
	}
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + host + path
}

// ParseTokens splits a comma separated token list, trimming blanks.
func ParseTokens(v string) []string { return ParseList(v) }

// ParseList splits a comma separated list, dropping empty entries.
func ParseList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseSize converts a human readable size ("100MB", "2GB", "512kb", "1048576")
// into bytes. KB/MB/GB/TB are binary multiples (1024-based), matching how
// operators think about disk quotas.
func ParseSize(v string) (int64, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, errors.New("empty size")
	}
	upper := strings.ToUpper(s)
	multipliers := []struct {
		suffix string
		mult   int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	}
	mult := int64(1)
	for _, m := range multipliers {
		if strings.HasSuffix(upper, m.suffix) {
			mult = m.mult
			upper = strings.TrimSpace(strings.TrimSuffix(upper, m.suffix))
			break
		}
	}
	if upper == "" {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	n, err := strconv.ParseFloat(upper, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", v)
	}
	return int64(n * float64(mult)), nil
}

// ParseDuration extends time.ParseDuration with a day suffix ("30d"), which
// operators reach for far more often than "720h".
func ParseDuration(v string) (time.Duration, error) {
	s := strings.TrimSpace(strings.ToLower(v))
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", v)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return d, nil
}

// ParseRate parses "60/m", "10/s" or "100/h" into a Rate.
func ParseRate(v string) (*Rate, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return nil, nil
	}
	n, unit, ok := strings.Cut(s, "/")
	if !ok {
		return nil, fmt.Errorf("invalid rate %q: expected N/s, N/m or N/h", v)
	}
	count, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || count <= 0 {
		return nil, fmt.Errorf("invalid rate %q: count must be a positive integer", v)
	}
	var period time.Duration
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "s", "sec", "second":
		period = time.Second
	case "m", "min", "minute":
		period = time.Minute
	case "h", "hour":
		period = time.Hour
	default:
		return nil, fmt.Errorf("invalid rate %q: unit must be s, m or h", v)
	}
	return &Rate{N: count, Period: period}, nil
}

// FormatSize renders a byte count using binary units, for logs and the CLI.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

func str(env Getenv, key, def string) string {
	if v := strings.TrimSpace(env(key)); v != "" {
		return v
	}
	return def
}

func sizeOr(env Getenv, key string, def int64) (int64, error) {
	v := strings.TrimSpace(env(key))
	if v == "" {
		return def, nil
	}
	return ParseSize(v)
}

func intOr(env Getenv, key string, def int) (int, error) {
	v := strings.TrimSpace(env(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", v)
	}
	return n, nil
}

func durationOr(env Getenv, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(env(key))
	if v == "" {
		return def, nil
	}
	if v == "0" {
		return 0, nil
	}
	return ParseDuration(v)
}

func boolOr(env Getenv, key string, def bool) (bool, error) {
	v := strings.TrimSpace(env(key))
	if v == "" {
		return def, nil
	}
	b, err := parseBool(v)
	if err != nil {
		return false, err
	}
	return b, nil
}

func telemetryOr(env Getenv) (bool, error) {
	v := strings.TrimSpace(env("GODROP_TELEMETRY"))
	if v == "" {
		return true, nil
	}
	return parseBool(v)
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "yes", "y", "on", "enabled":
		return true, nil
	case "0", "f", "false", "no", "n", "off", "disabled":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", v)
}

func rateOr(env Getenv, key string) (*Rate, error) {
	return ParseRate(env(key))
}

func parseLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid level %q: use debug, info, warn or error", v)
}
