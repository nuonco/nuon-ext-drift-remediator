package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/nuonco/nuon-ext-drift-remediator/internal/nuonapi"
	"github.com/nuonco/nuon-ext-drift-remediator/internal/server"
)

type serveOptions struct {
	addr            string
	webhookPath     string
	healthPath      string
	expectedSecret  string
	apiURL          string
	apiToken        string
	orgID           string
	dryRun          bool
	shutdownTimeout time.Duration
	readTimeout     time.Duration
	writeTimeout    time.Duration
	maxBodyBytes    int64
	prettyLogs      bool
}

func newServeCommand() *cobra.Command {
	opts := serveOptions{
		addr:            ":8788",
		webhookPath:     "/webhooks/drift",
		healthPath:      "/healthz",
		apiURL:          "https://api.nuon.co",
		shutdownTimeout: 10 * time.Second,
		readTimeout:     15 * time.Second,
		writeTimeout:    30 * time.Second,
		maxBodyBytes:    1024 * 1024,
	}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the drift-remediator webhook receiver",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.addr, "addr", opts.addr, "Address to bind the HTTP server to")
	cmd.Flags().StringVar(&opts.webhookPath, "path", opts.webhookPath, "Webhook endpoint path")
	cmd.Flags().StringVar(&opts.healthPath, "health-path", opts.healthPath, "Health check endpoint path")
	cmd.Flags().StringVar(&opts.expectedSecret, "secret", "", "Shared secret for HMAC-SHA256 signature validation (X-Nuon-Signature). Falls back to NUON_WEBHOOK_SECRET")
	cmd.Flags().StringVar(&opts.apiURL, "api-url", opts.apiURL, "Nuon API base URL. Falls back to NUON_API_URL")
	cmd.Flags().StringVar(&opts.apiToken, "api-token", "", "Nuon API token. Falls back to NUON_API_TOKEN")
	cmd.Flags().StringVar(&opts.orgID, "org-id", "", "Nuon org ID. When unset, the value is taken from each webhook's nuonorgid header. Falls back to NUON_ORG_ID")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Log remediation decisions but skip the API call")
	cmd.Flags().DurationVar(&opts.shutdownTimeout, "shutdown-timeout", opts.shutdownTimeout, "Graceful shutdown timeout")
	cmd.Flags().DurationVar(&opts.readTimeout, "read-timeout", opts.readTimeout, "HTTP read timeout")
	cmd.Flags().DurationVar(&opts.writeTimeout, "write-timeout", opts.writeTimeout, "HTTP write timeout")
	cmd.Flags().Int64Var(&opts.maxBodyBytes, "max-body-bytes", opts.maxBodyBytes, "Maximum accepted webhook payload size in bytes")
	cmd.Flags().BoolVar(&opts.prettyLogs, "pretty-logs", false, "Use human-readable development logs")

	return cmd
}

func runServe(parentCtx context.Context, opts serveOptions) error {
	if opts.expectedSecret == "" {
		opts.expectedSecret = strings.TrimSpace(os.Getenv("NUON_WEBHOOK_SECRET"))
	}
	if opts.apiToken == "" {
		opts.apiToken = strings.TrimSpace(os.Getenv("NUON_API_TOKEN"))
	}
	if envURL := strings.TrimSpace(os.Getenv("NUON_API_URL")); envURL != "" && opts.apiURL == "https://api.nuon.co" {
		opts.apiURL = envURL
	}
	if opts.orgID == "" {
		opts.orgID = strings.TrimSpace(os.Getenv("NUON_ORG_ID"))
	}

	logger, err := buildLogger(opts.prettyLogs)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	if opts.apiToken == "" && !opts.dryRun {
		logger.Warn("no API token configured (NUON_API_TOKEN or --api-token); remediation calls will fail with 401")
	}

	apiClient := nuonapi.New(nuonapi.Config{
		Logger:   logger.Named("api"),
		BaseURL:  opts.apiURL,
		APIToken: opts.apiToken,
		OrgID:    opts.orgID,
	})

	srv := server.New(server.Config{
		Logger:         logger,
		WebhookPath:    normalizePath(opts.webhookPath),
		HealthPath:     normalizePath(opts.healthPath),
		ExpectedSecret: opts.expectedSecret,
		MaxBodyBytes:   opts.maxBodyBytes,
		Remediator:     apiClient,
		DryRun:         opts.dryRun,
	})

	httpServer := &http.Server{
		Addr:              opts.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       opts.readTimeout,
		WriteTimeout:      opts.writeTimeout,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	logger.Info("drift-remediator webhook server started",
		zap.String("addr", opts.addr),
		zap.String("webhook_path", normalizePath(opts.webhookPath)),
		zap.String("health_path", normalizePath(opts.healthPath)),
		zap.String("api_url", opts.apiURL),
		zap.String("org_id", redactedOrgID(opts.orgID)),
		zap.Bool("secret_required", opts.expectedSecret != ""),
		zap.Bool("dry_run", opts.dryRun),
		zap.Int64("max_body_bytes", opts.maxBodyBytes))

	signalCtx, stopSignals := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	select {
	case <-signalCtx.Done():
		logger.Info("received shutdown signal")
	case err := <-errCh:
		if err != nil {
			logger.Error("webhook server exited with error", zap.Error(err))
			return err
		}
		logger.Info("webhook server stopped")
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", zap.Error(err))
		return err
	}

	if err := <-errCh; err != nil {
		logger.Error("webhook server exited with error", zap.Error(err))
		return err
	}

	logger.Info("webhook server stopped")
	return nil
}

func buildLogger(pretty bool) (*zap.Logger, error) {
	if pretty {
		cfg := zap.NewDevelopmentConfig()
		cfg.DisableCaller = true
		return cfg.Build()
	}
	cfg := zap.NewProductionConfig()
	return cfg.Build()
}

func normalizePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "/"
	}
	if strings.HasPrefix(trimmed, "/") {
		return trimmed
	}
	return "/" + trimmed
}

// redactedOrgID returns a short prefix of the org id for log output without
// leaking the full identifier.
func redactedOrgID(id string) string {
	if id == "" {
		return "(per-event)"
	}
	if len(id) <= 6 {
		return id
	}
	return id[:6] + "…"
}
