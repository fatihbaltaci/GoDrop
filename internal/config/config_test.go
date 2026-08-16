package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// envMap turns a map into a Getenv function, so tests never touch the real
// process environment and can run in parallel.
func envMap(m map[string]string) Getenv {
	return func(key string) string { return m[key] }
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFrom(envMap(map[string]string{"GODROP_TOKENS": "abc"}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.MaxFileSize != DefaultMaxFileSize {
		t.Errorf("MaxFileSize = %d, want %d", cfg.MaxFileSize, DefaultMaxFileSize)
	}
	if cfg.MaxTotalSize != 0 {
		t.Errorf("MaxTotalSize = %d, want 0 (unlimited)", cfg.MaxTotalSize)
	}
	if cfg.Retention != 0 {
		t.Errorf("Retention = %v, want 0 (keep forever)", cfg.Retention)
	}
	if cfg.CacheMaxAge != DefaultCacheMaxAge {
		t.Errorf("CacheMaxAge = %v, want a year", cfg.CacheMaxAge)
	}
	if cfg.RateLimit != nil || cfg.AuthRateLimit != nil {
		t.Error("rate limits should be disabled by default")
	}
	if cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 {
		t.Error("body timeouts must default to 0 so slow clients can finish large transfers")
	}
	if cfg.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v", cfg.ReadHeaderTimeout)
	}
	if !cfg.AccessLog || !cfg.Telemetry {
		t.Error("access log and telemetry default to on")
	}
	if len(cfg.CORSOrigins) != 1 || cfg.CORSOrigins[0] != "*" {
		t.Errorf("CORSOrigins = %v, want [*]", cfg.CORSOrigins)
	}
	if cfg.LogFormat != "json" || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("log defaults = %s/%v", cfg.LogFormat, cfg.LogLevel)
	}
}

func TestLoadWithoutTokensIsAllowed(t *testing.T) {
	t.Parallel()
	// Tokens may live in the token file instead; the server, not the config,
	// enforces that at least one exists.
	cfg, err := LoadFrom(envMap(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.Tokens) != 0 {
		t.Errorf("Tokens = %v, want empty", cfg.Tokens)
	}
}

func TestLoadFullEnvironment(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFrom(envMap(map[string]string{
		"GODROP_TOKENS":                " one , two ,, three ",
		"GODROP_ADDR":                  "127.0.0.1:9000",
		"GODROP_DATA_DIR":              "/srv/godrop",
		"GODROP_BASE_URL":              "https://files.example.com/",
		"GODROP_MAX_FILE_SIZE":         "250MB",
		"GODROP_MAX_FILES_PER_REQUEST": "5",
		"GODROP_MAX_TOTAL_SIZE":        "2GB",
		"GODROP_RETENTION":             "30d",
		"GODROP_CACHE_MAX_AGE":         "1h",
		"GODROP_RATE_LIMIT":            "60/m",
		"GODROP_AUTH_RATE_LIMIT":       "10/m",
		"GODROP_READ_HEADER_TIMEOUT":   "5s",
		"GODROP_READ_TIMEOUT":          "1m",
		"GODROP_WRITE_TIMEOUT":         "2m",
		"GODROP_IDLE_TIMEOUT":          "90s",
		"GODROP_SHUTDOWN_TIMEOUT":      "15s",
		"GODROP_CORS_ORIGINS":          "https://a.example.com, https://b.example.com",
		"GODROP_LOG_FORMAT":            "TEXT",
		"GODROP_LOG_LEVEL":             "debug",
		"GODROP_ACCESS_LOG":            "false",
		"GODROP_TELEMETRY":             "off",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got, want := strings.Join(cfg.Tokens, ","), "one,two,three"; got != want {
		t.Errorf("Tokens = %q, want %q", got, want)
	}
	if cfg.BaseURL != "https://files.example.com" {
		t.Errorf("BaseURL = %q, trailing slash should be trimmed", cfg.BaseURL)
	}
	if cfg.MaxFileSize != 250<<20 {
		t.Errorf("MaxFileSize = %d", cfg.MaxFileSize)
	}
	if cfg.MaxFilesPerRequest != 5 {
		t.Errorf("MaxFilesPerRequest = %d", cfg.MaxFilesPerRequest)
	}
	if cfg.MaxTotalSize != 2<<30 {
		t.Errorf("MaxTotalSize = %d", cfg.MaxTotalSize)
	}
	if cfg.Retention != 30*24*time.Hour {
		t.Errorf("Retention = %v", cfg.Retention)
	}
	if cfg.CacheMaxAge != time.Hour {
		t.Errorf("CacheMaxAge = %v", cfg.CacheMaxAge)
	}
	if cfg.RateLimit == nil || cfg.RateLimit.N != 60 || cfg.RateLimit.Period != time.Minute {
		t.Errorf("RateLimit = %+v", cfg.RateLimit)
	}
	if cfg.AuthRateLimit == nil || cfg.AuthRateLimit.N != 10 {
		t.Errorf("AuthRateLimit = %+v", cfg.AuthRateLimit)
	}
	if cfg.ReadTimeout != time.Minute || cfg.WriteTimeout != 2*time.Minute ||
		cfg.IdleTimeout != 90*time.Second || cfg.ShutdownTimeout != 15*time.Second {
		t.Error("timeouts were not parsed as expected")
	}
	if len(cfg.CORSOrigins) != 2 {
		t.Errorf("CORSOrigins = %v", cfg.CORSOrigins)
	}
	if cfg.LogFormat != "text" || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("log = %s/%v", cfg.LogFormat, cfg.LogLevel)
	}
	if cfg.AccessLog || cfg.Telemetry {
		t.Error("access log and telemetry should be off")
	}
}

func TestLoadInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad size", map[string]string{"GODROP_MAX_FILE_SIZE": "lots"}, "GODROP_MAX_FILE_SIZE"},
		{"zero size", map[string]string{"GODROP_MAX_FILE_SIZE": "0"}, "greater than zero"},
		{"bad quota", map[string]string{"GODROP_MAX_TOTAL_SIZE": "2 elephants"}, "GODROP_MAX_TOTAL_SIZE"},
		{"bad count", map[string]string{"GODROP_MAX_FILES_PER_REQUEST": "many"}, "GODROP_MAX_FILES_PER_REQUEST"},
		{"zero count", map[string]string{"GODROP_MAX_FILES_PER_REQUEST": "0"}, "greater than zero"},
		{"negative count", map[string]string{"GODROP_MAX_FILES_PER_REQUEST": "-2"}, "greater than zero"},
		{"bad retention", map[string]string{"GODROP_RETENTION": "forever"}, "GODROP_RETENTION"},
		{"negative retention", map[string]string{"GODROP_RETENTION": "-5h"}, "must not be negative"},
		{"bad cache age", map[string]string{"GODROP_CACHE_MAX_AGE": "a while"}, "GODROP_CACHE_MAX_AGE"},
		{"negative cache age", map[string]string{"GODROP_CACHE_MAX_AGE": "-1h"}, "must not be negative"},
		{"bad rate", map[string]string{"GODROP_RATE_LIMIT": "60"}, "expected N/s"},
		{"bad rate unit", map[string]string{"GODROP_RATE_LIMIT": "60/fortnight"}, "unit must be"},
		{"bad rate count", map[string]string{"GODROP_RATE_LIMIT": "zero/m"}, "positive integer"},
		{"zero rate count", map[string]string{"GODROP_RATE_LIMIT": "0/m"}, "positive integer"},
		{"bad auth rate", map[string]string{"GODROP_AUTH_RATE_LIMIT": "x/y"}, "GODROP_AUTH_RATE_LIMIT"},
		{"bad timeout", map[string]string{"GODROP_IDLE_TIMEOUT": "soon"}, "GODROP_IDLE_TIMEOUT"},
		{"negative timeout", map[string]string{"GODROP_WRITE_TIMEOUT": "-1s"}, "must not be negative"},
		{"bad log format", map[string]string{"GODROP_LOG_FORMAT": "xml"}, "json"},
		{"bad log level", map[string]string{"GODROP_LOG_LEVEL": "loud"}, "GODROP_LOG_LEVEL"},
		{"bad access log", map[string]string{"GODROP_ACCESS_LOG": "maybe"}, "GODROP_ACCESS_LOG"},
		{"bad telemetry", map[string]string{"GODROP_TELEMETRY": "sometimes"}, "GODROP_TELEMETRY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.env["GODROP_TOKENS"] = "abc"
			_, err := LoadFrom(envMap(tt.env))
			if err == nil {
				t.Fatalf("expected an error for %v", tt.env)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	// Fixing configuration one error per restart is miserable, so Load reports
	// all of them together.
	_, err := LoadFrom(envMap(map[string]string{
		"GODROP_TOKENS":        "abc",
		"GODROP_MAX_FILE_SIZE": "huge",
		"GODROP_LOG_FORMAT":    "xml",
		"GODROP_RETENTION":     "soon",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"GODROP_MAX_FILE_SIZE", "GODROP_LOG_FORMAT", "GODROP_RETENTION"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestLoadUsesProcessEnvironment(t *testing.T) {
	t.Setenv("GODROP_TOKENS", "from-process-env")
	t.Setenv("GODROP_ADDR", ":9999")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tokens) != 1 || cfg.Tokens[0] != "from-process-env" || cfg.Addr != ":9999" {
		t.Errorf("Load did not read the process environment: %+v", cfg)
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"1B", 1},
		{"1KB", 1 << 10},
		{"1kb", 1 << 10},
		{"1K", 1 << 10},
		{"1KiB", 1 << 10},
		{"100MB", 100 << 20},
		{" 2GB ", 2 << 30},
		{"1TB", 1 << 40},
		{"1.5MB", 1572864},
		{"0", 0},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"", "   ", "MB", "-5MB", "5 MB extra", "abc"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0.5d", 12 * time.Hour},
		{"12h", 12 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"", "  ", "xd", "soon", "30days"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) should fail", bad)
		}
	}
}

func TestParseRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in     string
		n      int
		period time.Duration
	}{
		{"10/s", 10, time.Second},
		{"10/sec", 10, time.Second},
		{"10/second", 10, time.Second},
		{"60/m", 60, time.Minute},
		{"60/min", 60, time.Minute},
		{"60/minute", 60, time.Minute},
		{"100/h", 100, time.Hour},
		{"100/hour", 100, time.Hour},
		{" 5 / M ", 5, time.Minute},
	}
	for _, tt := range tests {
		got, err := ParseRate(tt.in)
		if err != nil {
			t.Errorf("ParseRate(%q): %v", tt.in, err)
			continue
		}
		if got.N != tt.n || got.Period != tt.period {
			t.Errorf("ParseRate(%q) = %+v, want %d per %v", tt.in, got, tt.n, tt.period)
		}
	}
	if r, err := ParseRate("  "); err != nil || r != nil {
		t.Errorf("empty rate should mean disabled, got %+v %v", r, err)
	}
}

func TestRateString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rate Rate
		want string
	}{
		{Rate{10, time.Second}, "10/s"},
		{Rate{60, time.Minute}, "60/m"},
		{Rate{100, time.Hour}, "100/h"},
		{Rate{3, 2 * time.Hour}, "3/2h0m0s"},
	}
	for _, tt := range tests {
		if got := tt.rate.String(); got != tt.want {
			t.Errorf("%+v.String() = %q, want %q", tt.rate, got, tt.want)
		}
	}
}

func TestPublicURL(t *testing.T) {
	t.Parallel()
	withBase := &Config{BaseURL: "https://files.example.com"}
	if got := withBase.PublicURL("http", "localhost:8080", "/f/x.jpg"); got != "https://files.example.com/f/x.jpg" {
		t.Errorf("configured base URL should win, got %q", got)
	}
	derived := &Config{}
	if got := derived.PublicURL("https", "files.example.com", "/f/x.jpg"); got != "https://files.example.com/f/x.jpg" {
		t.Errorf("PublicURL = %q", got)
	}
	if got := derived.PublicURL("", "localhost:8080", "/f/x.jpg"); got != "http://localhost:8080/f/x.jpg" {
		t.Errorf("missing scheme should fall back to http, got %q", got)
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1 << 10, "1.0KB"},
		{100 << 20, "100.0MB"},
		{2 << 30, "2.0GB"},
		{3 << 40, "3.0TB"},
	}
	for _, tt := range tests {
		if got := FormatSize(tt.in); got != tt.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseListDropsBlanks(t *testing.T) {
	t.Parallel()
	got := ParseList(" a ,, b,, ,c ")
	if strings.Join(got, "|") != "a|b|c" {
		t.Errorf("ParseList = %v", got)
	}
	if ParseList("") != nil {
		t.Error("empty input should produce a nil slice")
	}
}

func TestParseBoolForms(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"1", "t", "true", "YES", "y", "on", "enabled"} {
		cfg, err := LoadFrom(envMap(map[string]string{"GODROP_TOKENS": "x", "GODROP_ACCESS_LOG": v}))
		if err != nil || !cfg.AccessLog {
			t.Errorf("%q should parse as true (err=%v)", v, err)
		}
	}
	for _, v := range []string{"0", "f", "false", "NO", "n", "off", "disabled"} {
		cfg, err := LoadFrom(envMap(map[string]string{"GODROP_TOKENS": "x", "GODROP_ACCESS_LOG": v}))
		if err != nil || cfg.AccessLog {
			t.Errorf("%q should parse as false (err=%v)", v, err)
		}
	}
}

func TestExplicitZeroTimeout(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFrom(envMap(map[string]string{
		"GODROP_TOKENS":              "x",
		"GODROP_READ_HEADER_TIMEOUT": "0",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.ReadHeaderTimeout != 0 {
		t.Errorf("an explicit 0 must disable the timeout, got %v", cfg.ReadHeaderTimeout)
	}
}

func TestLogLevels(t *testing.T) {
	t.Parallel()
	levels := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError,
	}
	for in, want := range levels {
		cfg, err := LoadFrom(envMap(map[string]string{"GODROP_TOKENS": "x", "GODROP_LOG_LEVEL": in}))
		if err != nil {
			t.Fatalf("LoadFrom(%q): %v", in, err)
		}
		if cfg.LogLevel != want {
			t.Errorf("level %q = %v, want %v", in, cfg.LogLevel, want)
		}
	}
}
