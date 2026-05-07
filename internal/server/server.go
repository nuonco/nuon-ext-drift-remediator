// Package server is the HTTP handler that receives ctl-api lifecycle webhooks,
// filters them down to drift-detected events, and dispatches remediation via
// a Remediator.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/nuonco/nuon-ext-drift-remediator/internal/payload"
)

const defaultMaxBodyBytes int64 = 1024 * 1024

// Remediator is the callout interface implemented by internal/nuonapi.Client.
// Defined here (not in nuonapi) so the package is consumable without an api
// client in tests.
type Remediator interface {
	Remediate(ctx context.Context, target payload.DriftTarget) error
}

// Config bundles the runtime knobs. WebhookPath / HealthPath default to
// sensible values when empty.
type Config struct {
	Logger         *zap.Logger
	WebhookPath    string
	HealthPath     string
	ExpectedSecret string
	MaxBodyBytes   int64
	Remediator     Remediator
	DryRun         bool
}

type Server struct {
	logger         *zap.Logger
	webhookPath    string
	healthPath     string
	expectedSecret string
	maxBodyBytes   int64
	remediator     Remediator
	dryRun         bool
	handler        http.Handler
}

// ackResponse is what the receiver returns to ctl-api on every accepted
// payload. The shape mirrors nuon-ext-webhook-server for symmetry, with an
// additional `remediated` field that records whether a remediation call was
// dispatched (false for dry-run / non-drift events).
type ackResponse struct {
	OK         bool   `json:"ok"`
	ReceivedAt string `json:"received_at"`
	EventID    string `json:"event_id,omitempty"`
	Drift      bool   `json:"drift"`
	Remediated bool   `json:"remediated"`
	Action     string `json:"action,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	webhookPath := normalizePath(cfg.WebhookPath)
	healthPath := normalizePath(cfg.HealthPath)
	if webhookPath == "/" {
		webhookPath = "/webhooks/drift"
	}
	if healthPath == "/" {
		healthPath = "/healthz"
	}

	maxBodyBytes := cfg.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}

	s := &Server{
		logger:         logger,
		webhookPath:    webhookPath,
		healthPath:     healthPath,
		expectedSecret: strings.TrimSpace(cfg.ExpectedSecret),
		maxBodyBytes:   maxBodyBytes,
		remediator:     cfg.Remediator,
		dryRun:         cfg.DryRun,
	}

	mux := http.NewServeMux()
	mux.HandleFunc(s.webhookPath, s.handleWebhook)
	mux.HandleFunc(s.healthPath, s.handleHealth)
	s.handler = mux

	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, ackResponse{OK: true, ReceivedAt: now()})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Warn("unable to read webhook body",
			zap.String("remote_addr", requestIP(r)),
			zap.Error(err))
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unable to read request body"})
		return
	}

	if s.expectedSecret != "" {
		providedSig := strings.TrimSpace(r.Header.Get("X-Nuon-Signature"))
		if providedSig == "" {
			s.logger.Warn("rejecting webhook: missing X-Nuon-Signature",
				zap.String("remote_addr", requestIP(r)))
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing signature"})
			return
		}

		mac := hmac.New(sha256.New, []byte(s.expectedSecret))
		mac.Write(body)
		expectedSig := hex.EncodeToString(mac.Sum(nil))

		if !hmac.Equal([]byte(providedSig), []byte(expectedSig)) {
			s.logger.Warn("rejecting webhook: signature mismatch",
				zap.String("remote_addr", requestIP(r)))
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid signature"})
			return
		}
	}

	var event payload.CloudEvent
	if err := json.Unmarshal(body, &event); err != nil {
		s.logger.Warn("unable to decode webhook payload",
			zap.String("remote_addr", requestIP(r)),
			zap.Error(err))
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON payload"})
		return
	}

	if err := event.Validate(); err != nil {
		s.logger.Warn("invalid webhook payload",
			zap.String("remote_addr", requestIP(r)),
			zap.Error(err))
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	// Non-drift events are accepted (200) so ctl-api stops retrying, but
	// we don't take any action. ctl-api fans out a single CloudEvent to
	// every subscribed webhook — the matcher already filtered by interest
	// slug, so receiving a non-drift event here only happens when the
	// operator subscribed to a broader interest set than just drift.
	if !event.IsDrift() {
		s.logger.Debug("ignoring non-drift event",
			zap.String("event_id", event.ID),
			zap.Strings("interests", event.Interests))
		writeJSON(w, http.StatusOK, ackResponse{
			OK:         true,
			ReceivedAt: now(),
			EventID:    event.ID,
			Reason:     "not a drift event",
		})
		return
	}

	target, reason, ok := event.ResolveDriftTarget()
	if !ok {
		s.logger.Warn("drift event missing remediation target",
			zap.String("event_id", event.ID),
			zap.String("reason", reason))
		// Return 200 so ctl-api doesn't retry an unfixable event.
		writeJSON(w, http.StatusOK, ackResponse{
			OK:         true,
			ReceivedAt: now(),
			EventID:    event.ID,
			Drift:      true,
			Reason:     "unable to resolve remediation target: " + reason,
		})
		return
	}

	action := remediationAction(target)
	logger := s.logger.With(
		zap.String("event_id", event.ID),
		zap.String("install_id", target.InstallID),
		zap.String("kind", string(target.Kind)),
		zap.String("component_id", target.ComponentID),
		zap.String("sandbox_id", target.SandboxID),
		zap.String("action", action),
	)

	if s.dryRun {
		logger.Info("dry-run: skipping remediation API call")
		writeJSON(w, http.StatusOK, ackResponse{
			OK:         true,
			ReceivedAt: now(),
			EventID:    event.ID,
			Drift:      true,
			Remediated: false,
			Action:     action,
			Reason:     "dry-run",
		})
		return
	}

	if s.remediator == nil {
		logger.Error("no remediator configured; cannot remediate")
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "remediator not configured"})
		return
	}

	logger.Info("remediating drift")
	if err := s.remediator.Remediate(r.Context(), target); err != nil {
		logger.Error("remediation failed", zap.Error(err))
		// Return 5xx so ctl-api retries the webhook delivery (and we
		// re-attempt remediation) instead of silently dropping it.
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error: "remediation failed: " + err.Error(),
		})
		return
	}

	logger.Info("remediation dispatched")
	writeJSON(w, http.StatusOK, ackResponse{
		OK:         true,
		ReceivedAt: now(),
		EventID:    event.ID,
		Drift:      true,
		Remediated: true,
		Action:     action,
	})
}

// remediationAction returns a short human-readable label describing what
// the remediator would do for a target. Used for logs and ack responses.
func remediationAction(target payload.DriftTarget) string {
	switch target.Kind {
	case payload.DriftKindSandbox:
		return "reprovision-sandbox"
	case payload.DriftKindComponent:
		return "redeploy-component"
	default:
		return string(target.Kind)
	}
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func requestIP(r *http.Request) string {
	if forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
