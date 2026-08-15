package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/fatihbaltaci/GoDrop/internal/config"
	"github.com/fatihbaltaci/GoDrop/internal/server"
	"github.com/fatihbaltaci/GoDrop/internal/storage"
	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
	"github.com/fatihbaltaci/GoDrop/internal/tokens"
)

func newServeCmd(build Build) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runServe(cmd, build) },
	}
}

func runServe(cmd *cobra.Command, build Build) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	logger := newLogger(cfg, cmd.OutOrStdout())

	store, err := storage.New(cfg.DataDir, cfg.MaxTotalSize)
	if err != nil {
		return err
	}
	tokenStore, err := tokens.New(tokens.Path(store.Root()), cfg.Tokens)
	if err != nil {
		return err
	}
	// Reloading the token file fails open so a transient problem cannot lock
	// every client out, which makes saying so out loud the only warning the
	// operator gets: until it is fixed, a revoked token stays valid.
	tokenStore.SetErrorHandler(func(err error) {
		logger.Error("token file could not be reloaded; still using the last good copy", "err", err.Error())
	})
	if tokenStore.Count() == 0 {
		return errors.New(`no API tokens configured — refusing to start with unauthenticated uploads.

  Create one:   godrop token create --name default
  Or set:       GODROP_TOKENS=<token>            (handy on Docker, Fly, Railway)
  Or run:       godrop init                      (guided setup)`)
	}

	srv := server.New(server.Options{
		Config:  cfg,
		Store:   store,
		Tokens:  tokenStore,
		Logger:  logger,
		Version: build.Version,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runCleanup(ctx, store, cfg, logger)
	go flushTokens(ctx, tokenStore, logger)

	tel, err := telemetry.New(telemetry.Options{
		Key:     build.PostHogKey,
		Host:    build.PostHogHost,
		Version: build.Version,
		DataDir: store.Root(),
		Logger:  logger,
	})
	if err != nil {
		logger.Warn("telemetry disabled", "err", err.Error())
	}
	switch {
	case tel == nil:
		logger.Debug("telemetry inactive (no key compiled in)")
	case !cfg.Telemetry || telemetry.Disabled(store.Root()):
		logger.Info("telemetry disabled")
	default:
		logger.Info("anonymous telemetry is on — install id, version and platform only; disable with GODROP_TELEMETRY=off")
		go tel.Run(ctx)
	}

	logStartup(logger, cfg, store, tokenStore, build)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := tokenStore.Flush(); err != nil {
		logger.Warn("could not save token usage", "err", err.Error())
	}
	return httpSrv.Shutdown(shutdownCtx)
}

// newLogger writes to the command's output, which is os.Stdout in production
// and something a test can read in the suite.
func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func logStartup(logger *slog.Logger, cfg *config.Config, store *storage.Store, ts *tokens.Store, build Build) {
	files, bytes := store.Stats()
	attrs := []any{
		"addr", cfg.Addr,
		"version", build.Version,
		"data_dir", store.Root(),
		"files", files,
		"bytes", bytes,
		"max_file", config.FormatSize(cfg.MaxFileSize),
		"tokens", ts.Count(),
	}
	if cfg.BaseURL != "" {
		attrs = append(attrs, "base_url", cfg.BaseURL)
	}
	if cfg.MaxTotalSize > 0 {
		attrs = append(attrs, "quota", config.FormatSize(cfg.MaxTotalSize))
	}
	if cfg.Retention > 0 {
		attrs = append(attrs, "retention", cfg.Retention.String())
	}
	logger.Info("godrop listening", attrs...)
}

// runCleanup deletes expired files on a timer when retention is configured.
func runCleanup(ctx context.Context, store *storage.Store, cfg *config.Config, logger *slog.Logger) {
	if cfg.Retention <= 0 {
		return
	}
	interval := time.Hour
	if cfg.Retention < interval {
		interval = cfg.Retention
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		removed, freed, err := store.Cleanup(cfg.Retention)
		switch {
		case err != nil:
			logger.Error("cleanup failed", "err", err.Error())
		case removed > 0:
			logger.Info("cleanup", "removed", removed, "freed", config.FormatSize(freed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// flushInterval bounds how often token usage is written to disk.
var flushInterval = time.Minute

// flushTokens persists last-used timestamps periodically, so recording token
// usage costs no disk write per request.
func flushTokens(ctx context.Context, ts *tokens.Store, logger *slog.Logger) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ts.Flush(); err != nil {
				logger.Warn("could not save token usage", "err", err.Error())
			}
		}
	}
}
