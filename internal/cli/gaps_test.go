package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/doctor"
	"github.com/fatihbaltaci/GoDrop/internal/netcheck"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

func TestExecuteWritesToTheProcessStreams(t *testing.T) {
	// Execute is what main() calls; make sure the wiring works end to end.
	if code := Execute(context.Background(), testBuild(), []string{"version"}); code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestRootWithoutArgumentsServes(t *testing.T) {
	// A container image runs the bare binary, so the root command must be the
	// server. Here it fails for want of a token, which proves it got there.
	t.Setenv("GODROP_DATA_DIR", t.TempDir())
	t.Setenv("GODROP_TOKENS", "")
	code, _, stderr := run(t, testBuild())
	if code == 0 || !strings.Contains(stderr, "no API tokens") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestDoctorReportsAllStatuses(t *testing.T) {
	var buf bytes.Buffer
	out := &output{w: &buf}
	printReport(out, doctor.Report{
		OK: false,
		Checks: []doctor.Check{
			{Group: "config", Name: "tokens", Status: doctor.Pass, Detail: "1 token"},
			{Group: "storage", Name: "usage", Status: doctor.Warn, Detail: "nearly full", Fix: "raise the quota"},
			{Group: "security", Name: "https", Status: doctor.Fail, Detail: "plain http", Fix: "use TLS"},
			{Group: "network", Name: "external", Status: doctor.Skip, Detail: "offline"},
			{Group: "unknown-group", Name: "ignored", Status: doctor.Pass},
		},
	})
	text := buf.String()
	for _, want := range []string{"tokens", "nearly full", "raise the quota", "plain http", "use TLS", "offline", "check(s) failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("report should contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ignored") {
		t.Error("a check in an unknown group should not be printed")
	}

	buf.Reset()
	printReport(out, doctor.Report{OK: true, Checks: []doctor.Check{{Group: "config", Name: "tokens", Status: doctor.Pass}}})
	if !strings.Contains(buf.String(), "everything looks good") {
		t.Errorf("a clean report should say so:\n%s", buf.String())
	}
}

func TestTemporaryTokenIsRemovedAgain(t *testing.T) {
	dir := t.TempDir()
	cfg := mustConfig(t, map[string]string{"GODROP_DATA_DIR": dir})

	token, cleanup, err := temporaryToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, tokens.Prefix) {
		t.Errorf("token = %q", token)
	}
	store, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(token); !ok {
		t.Error("the temporary token should work while the diagnosis runs")
	}
	cleanup()

	store.Flush()
	fresh, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.List()) != 0 {
		t.Errorf("the temporary token was left behind: %+v", fresh.List())
	}
}

func TestTemporaryTokenPrefersAConfiguredOne(t *testing.T) {
	cfg := mustConfig(t, map[string]string{
		"GODROP_DATA_DIR": t.TempDir(),
		"GODROP_TOKENS":   "already-configured",
	})
	token, cleanup, err := temporaryToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if token != "already-configured" {
		t.Errorf("token = %q, want the configured one", token)
	}
}

func TestTemporaryTokenReportsFailures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(tokens.Path(dir), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, map[string]string{"GODROP_DATA_DIR": dir})
	if _, _, err := temporaryToken(cfg); err == nil {
		t.Error("a corrupt token store should be reported")
	}
}

func TestDataDirFallsBackToTheDefault(t *testing.T) {
	t.Setenv("GODROP_DATA_DIR", "")
	cmd := newTokenCmd(testBuild())
	if got := dataDir(cmd); got != "./data" {
		t.Errorf("dataDir = %q, want the documented default", got)
	}
}

// ---------------------------------------------------------------- verify

func TestVerifyReportsFirewallStates(t *testing.T) {
	originalFirewall, originalExternal := checkFirewall, externalCheck
	t.Cleanup(func() { checkFirewall, externalCheck = originalFirewall, originalExternal })

	a := wizard.Defaults()
	a.BaseURL = "https://files.example.com"
	a.Port = "1"

	tests := []struct {
		name string
		fw   netcheck.Firewall
		want string
	}{
		{"open", netcheck.Firewall{Tool: "ufw", Inspected: true, PortOpen: true}, "allows port"},
		{"blocked", netcheck.Firewall{Tool: "ufw", Inspected: true, Hint: "sudo ufw allow 443/tcp"}, "blocks port"},
		{"absent", netcheck.Firewall{}, "no host firewall"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkFirewall = func(context.Context, netcheck.Runner, int) netcheck.Firewall { return tt.fw }
			externalCheck = func(context.Context, *http.Client, string, string) (netcheck.ExternalResult, error) {
				return netcheck.ExternalResult{OK: true, Status: 200, Location: "FRA"}, nil
			}
			var buf bytes.Buffer
			verify(t.Context(), &output{w: &buf}, a)
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("output should mention %q:\n%s", tt.want, buf.String())
			}
			if !strings.Contains(buf.String(), "reachable") {
				t.Errorf("the external result should be reported:\n%s", buf.String())
			}
		})
	}
}

func TestVerifyReportsAnUnreachableServer(t *testing.T) {
	originalFirewall, originalExternal := checkFirewall, externalCheck
	t.Cleanup(func() { checkFirewall, externalCheck = originalFirewall, originalExternal })
	checkFirewall = func(context.Context, netcheck.Runner, int) netcheck.Firewall { return netcheck.Firewall{} }

	a := wizard.Defaults()
	a.BaseURL = "https://files.example.com"
	a.Port = "1"

	t.Run("refused", func(t *testing.T) {
		externalCheck = func(context.Context, *http.Client, string, string) (netcheck.ExternalResult, error) {
			return netcheck.ExternalResult{OK: false, Error: "connection refused"}, nil
		}
		var buf bytes.Buffer
		verify(t.Context(), &output{w: &buf}, a)
		if !strings.Contains(buf.String(), "not reachable") || !strings.Contains(buf.String(), "cloud provider") {
			t.Errorf("output = %s", buf.String())
		}
	})

	t.Run("status only", func(t *testing.T) {
		externalCheck = func(context.Context, *http.Client, string, string) (netcheck.ExternalResult, error) {
			return netcheck.ExternalResult{OK: false, Status: 502}, nil
		}
		var buf bytes.Buffer
		verify(t.Context(), &output{w: &buf}, a)
		if !strings.Contains(buf.String(), "502") {
			t.Errorf("output = %s", buf.String())
		}
	})

	t.Run("probe unavailable", func(t *testing.T) {
		externalCheck = func(context.Context, *http.Client, string, string) (netcheck.ExternalResult, error) {
			return netcheck.ExternalResult{}, errors.New("probe is down")
		}
		var buf bytes.Buffer
		verify(t.Context(), &output{w: &buf}, a)
		if !strings.Contains(buf.String(), "curl -sI") {
			t.Errorf("a failed probe should leave a manual command:\n%s", buf.String())
		}
	})
}

func TestVerifyDetectsAListeningServer(t *testing.T) {
	originalFirewall, originalExternal := checkFirewall, externalCheck
	t.Cleanup(func() { checkFirewall, externalCheck = originalFirewall, originalExternal })
	checkFirewall = func(context.Context, netcheck.Runner, int) netcheck.Firewall { return netcheck.Firewall{} }
	externalCheck = func(context.Context, *http.Client, string, string) (netcheck.ExternalResult, error) {
		return netcheck.ExternalResult{OK: true, Status: 200}, nil
	}

	addr := freePort(t)
	srv := &http.Server{Addr: addr, Handler: http.NewServeMux()}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	waitForListener(t, addr)

	a := wizard.Defaults()
	a.Port = strings.TrimPrefix(addr, "127.0.0.1:")
	a.BaseURL = ""

	var buf bytes.Buffer
	verify(t.Context(), &output{w: &buf}, a)
	if !strings.Contains(buf.String(), "listening") {
		t.Errorf("a running server should be detected:\n%s", buf.String())
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if netcheck.Listening(t.Context(), addr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing came up on %s", addr)
}

// ------------------------------------------------------------- maybeStart

// fakeDocker puts a stub docker command at the front of PATH.
func fakeDocker(t *testing.T, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a POSIX shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexit " + string(rune('0'+exitCode)) + "\n"
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestMaybeStartRunsCompose(t *testing.T) {
	fakeDocker(t, 0)
	var buf bytes.Buffer
	a := wizard.Defaults()
	if err := maybeStart(t.Context(), &output{w: &buf}, a, true); err != nil {
		t.Fatalf("maybeStart: %v", err)
	}
	if !strings.Contains(buf.String(), "containers started") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestMaybeStartReportsComposeFailure(t *testing.T) {
	fakeDocker(t, 1)
	err := maybeStart(t.Context(), &output{w: io.Discard}, wizard.Defaults(), true)
	if err == nil || !strings.Contains(err.Error(), "docker compose up failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestMaybeStartWithoutTheDockerBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var buf bytes.Buffer
	if err := maybeStart(t.Context(), &output{w: &buf}, wizard.Defaults(), true); err != nil {
		t.Fatalf("a missing docker should not be fatal: %v", err)
	}
	if !strings.Contains(buf.String(), "docker not found") {
		t.Errorf("output = %q", buf.String())
	}
}

// ------------------------------------------------------------------- ui

func TestColourIsOffWhenOutputIsNotATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if useColor(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
	if useColor(os.Stdout) && os.Getenv("CI") != "" {
		t.Error("CI output should not be coloured")
	}
}

func TestNoColorEnvironmentIsHonoured(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if useColor(os.Stdout) {
		t.Error("NO_COLOR must disable colour")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if useColor(os.Stdout) {
		t.Error("a dumb terminal gets no colour")
	}
}

func TestUseColorRejectsAClosedFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if useColor(f) {
		t.Error("a closed file cannot be a terminal")
	}
}

func TestTintOnlyPaintsWhenColourIsOn(t *testing.T) {
	plain := &output{w: io.Discard, color: false}
	if got := plain.tint(color.New(color.FgRed), "text"); got != "text" {
		t.Errorf("tint = %q, want the bare string", got)
	}
	painted := &output{w: io.Discard, color: true}
	if got := painted.tint(color.New(color.FgRed), "text"); !strings.Contains(got, "text") {
		t.Errorf("tint = %q", got)
	}
}

func TestErrorPrefix(t *testing.T) {
	if !strings.Contains(errorPrefix(), "error:") {
		t.Errorf("errorPrefix = %q", errorPrefix())
	}
}

func TestBoxFitsItsWidestLine(t *testing.T) {
	var buf bytes.Buffer
	(&output{w: &buf}).box("short", "a much longer line")
	text := buf.String()
	if !strings.Contains(text, "short") || !strings.Contains(text, "a much longer line") {
		t.Errorf("box = %q", text)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 4 {
		t.Errorf("a two-line box should have four rows, got %d", len(lines))
	}
}

func TestJSONModeSuppressesProse(t *testing.T) {
	var buf bytes.Buffer
	out := &output{w: &buf, json: true}
	out.heading("Heading")
	out.success("done")
	out.warn("careful")
	out.fail("broken")
	out.skip("skipped")
	out.hint("try this")
	out.command("godrop serve")
	out.box("token")
	if buf.Len() != 0 {
		t.Fatalf("--json output must contain nothing but the document:\n%s", buf.String())
	}
	if err := out.emit(map[string]string{"ok": "yes"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ok"`) {
		t.Errorf("the document itself should still be written: %q", buf.String())
	}
}

func TestInteractiveIsFalseInTests(t *testing.T) {
	// The test binary's streams are pipes, never a terminal.
	if interactive() {
		t.Error("interactive() should be false when stdin is not a terminal")
	}
}

// ------------------------------------------------------------------ serve

func TestFlushTokensWritesPendingUsage(t *testing.T) {
	original := flushInterval
	flushInterval = 10 * time.Millisecond
	t.Cleanup(func() { flushInterval = original })

	dir := t.TempDir()
	store, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("token should verify")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go flushTokens(ctx, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(tokens.Path(dir))
		if err == nil && strings.Contains(string(data), "last_used") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("token usage was never flushed to disk")
}

func TestFlushTokensLogsFailures(t *testing.T) {
	requireStrictPermissions(t)
	original := flushInterval
	flushInterval = 10 * time.Millisecond
	t.Cleanup(func() { flushInterval = original })

	dir := t.TempDir()
	store, err := tokens.New(tokens.Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := store.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("token should verify")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var logs safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		flushTokens(ctx, store, slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	// Wait for the loop to stop before anything else is cleaned up: it writes
	// into the directory the test is about to remove, and one more flush
	// landing during the removal would fail the test for the wrong reason.
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "could not save token usage") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a failing flush should be logged:\n%s", logs.String())
}

// mustConfig loads a configuration from a fake environment.
func mustConfig(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
