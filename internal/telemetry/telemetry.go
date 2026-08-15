// Package telemetry sends an anonymous daily heartbeat so the project can tell
// how many installations exist, which versions are still running and which
// platforms deserve attention.
//
// What is sent: a random installation id, the GoDrop version, the operating
// system, the CPU architecture and the deployment type. Nothing else — no file
// names, no identifiers, no counters, no client addresses, no base URL.
//
// It can be switched off with GODROP_TELEMETRY=off or `godrop telemetry off`.
// It is also inert unless the binary was built with a PostHog key, so anything
// compiled from source or installed with `go install` never reports at all.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FileName stores the installation id inside the data directory.
const FileName = ".install_id"

// Interval between heartbeats.
const Interval = 24 * time.Hour

// Event is the single event name used.
const Event = "heartbeat"

// Client sends heartbeats. A nil Client is valid and does nothing.
type Client struct {
	key       string
	host      string
	version   string
	installID string
	deploy    string
	dataDir   string

	http *http.Client
	log  *slog.Logger
	now  func() time.Time
}

// Options configures New.
type Options struct {
	Key       string // PostHog project key; empty disables telemetry
	Host      string // e.g. https://eu.i.posthog.com
	Version   string
	DataDir   string
	Logger    *slog.Logger
	Env       func(string) string
	Client    *http.Client
	Now       func() time.Time
	InstallID string // overrides the persisted id (tests)
}

// New returns a Client, or nil when telemetry cannot or should not run.
func New(opts Options) (*Client, error) {
	if opts.Key == "" {
		return nil, nil
	}
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	httpClient := opts.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	id := opts.InstallID
	if id == "" {
		var err error
		if id, err = InstallID(opts.DataDir); err != nil {
			return nil, err
		}
	}
	host := strings.TrimRight(opts.Host, "/")
	if host == "" {
		host = "https://eu.i.posthog.com"
	}
	return &Client{
		key:       opts.Key,
		host:      host,
		version:   opts.Version,
		installID: id,
		deploy:    DetectDeploy(env),
		dataDir:   opts.DataDir,
		http:      httpClient,
		log:       log,
		now:       now,
	}, nil
}

// Payload is exactly what leaves the machine. It is exported so `godrop
// telemetry status` can show the user the real thing rather than a promise.
type Payload struct {
	APIKey     string         `json:"api_key"`
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
	Timestamp  string         `json:"timestamp"`
}

// Payload builds the heartbeat body.
func (c *Client) Payload() Payload {
	return Payload{
		APIKey:     c.key,
		Event:      Event,
		DistinctID: c.installID,
		Properties: map[string]any{
			"version": c.version,
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"deploy":  c.deploy,
		},
		Timestamp: c.now().UTC().Format(time.RFC3339),
	}
}

// Run sends a heartbeat immediately and then once per Interval until ctx ends.
// Failures are logged at debug level and never affect serving.
func (c *Client) Run(ctx context.Context) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		if err := c.Send(ctx); err != nil && !errors.Is(err, context.Canceled) {
			c.log.Debug("telemetry send failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Send delivers a single heartbeat, unless telemetry has been switched off in
// the meantime.
//
// The opt-out marker is read on every send rather than once at startup:
// `godrop telemetry off` promises that nothing more will be sent, not that
// nothing more will be sent after the next restart. Reading it back also means
// switching telemetry on again takes effect without one.
func (c *Client) Send(ctx context.Context) error {
	if c == nil || Disabled(c.dataDir) {
		return nil
	}
	// The payload is a fixed shape of strings, so encoding it cannot fail.
	// api_key is a PostHog project key, which is public by design — it ships in
	// client-side JavaScript wherever PostHog is used.
	body, _ := json.Marshal(c.Payload()) //nolint:gosec // G117
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/i/v0/e", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "godrop/"+c.version)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry endpoint returned %s", resp.Status)
	}
	return nil
}

// InstallID returns the persistent anonymous installation identifier, creating
// it on first use. It is a random value with no link to the host, the network
// or anything stored in GoDrop.
func InstallID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, FileName)
	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read install id: %w", err)
	}

	// Since Go 1.24 crypto/rand.Read is documented never to fail.
	buf := make([]byte, 16)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write install id: %w", err)
	}
	return id, nil
}

// OptOutFile marks telemetry as disabled for this installation. It lives beside
// the data so that the choice survives restarts and container recreation,
// whatever way the service is started.
const OptOutFile = ".telemetry-off"

// Disabled reports whether this installation has opted out.
func Disabled(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, OptOutFile))
	return err == nil
}

// SetDisabled records the opt-out choice.
func SetDisabled(dataDir string, disabled bool) error {
	path := filepath.Join(dataDir, OptOutFile)
	if !disabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("enable telemetry: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte("telemetry disabled by `godrop telemetry off`\n"), 0o600); err != nil {
		return fmt.Errorf("disable telemetry: %w", err)
	}
	return nil
}

// DetectDeploy guesses how GoDrop is being run, from environment markers set by
// the respective platforms.
func DetectDeploy(env func(string) string) string {
	switch {
	case env("FLY_APP_NAME") != "":
		return "fly"
	case env("RENDER") != "" || env("RENDER_SERVICE_ID") != "":
		return "render"
	case env("RAILWAY_ENVIRONMENT") != "" || env("RAILWAY_PROJECT_ID") != "":
		return "railway"
	case env("KUBERNETES_SERVICE_HOST") != "":
		return "kubernetes"
	case fileExists("/.dockerenv"):
		return "docker"
	case env("INVOCATION_ID") != "":
		return "systemd"
	default:
		return "binary"
	}
}

// fileExists is a variable so tests can simulate a container environment.
var fileExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
