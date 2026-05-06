// Package nuonapi is a thin HTTP client over the public Nuon API endpoints
// the drift-remediator needs. It is intentionally NOT a generated SDK; the
// extension only calls two endpoints and pulling in the full nuon-go SDK
// would balloon go.mod for no benefit.
package nuonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/nuonco/nuon-ext-drift-remediator/internal/payload"
)

const defaultHTTPTimeout = 30 * time.Second

// Config wires up the client. APIToken is required for any non-dry-run
// remediation; OrgID may be empty when the caller supplies it per-request.
type Config struct {
	Logger     *zap.Logger
	BaseURL    string
	APIToken   string
	OrgID      string
	HTTPClient *http.Client
}

// Client calls the public Nuon API for sandbox reprovision and component
// deploy operations. It satisfies the server.Remediator interface.
type Client struct {
	logger   *zap.Logger
	baseURL  string
	apiToken string
	orgID    string
	http     *http.Client
}

// New returns a Client wired with the provided config. Defaults: BaseURL
// "https://api.nuon.co", HTTP timeout 30s, no-op logger.
func New(cfg Config) *Client {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.nuon.co"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		logger:   logger,
		baseURL:  baseURL,
		apiToken: strings.TrimSpace(cfg.APIToken),
		orgID:    strings.TrimSpace(cfg.OrgID),
		http:     httpClient,
	}
}

// Remediate routes the target onto the correct endpoint. Returns nil on
// 2xx; otherwise an error with the response status and body excerpt.
func (c *Client) Remediate(ctx context.Context, target payload.DriftTarget) error {
	switch target.Kind {
	case payload.DriftKindSandbox:
		return c.ReprovisionSandbox(ctx, target.OrgID, target.InstallID)
	case payload.DriftKindComponent:
		return c.DeployComponent(ctx, target.OrgID, target.InstallID, target.ComponentID)
	default:
		return fmt.Errorf("unsupported drift kind %q", target.Kind)
	}
}

// ReprovisionSandbox triggers a full sandbox reprovision and redeploys all
// components on top.
//
//	POST /v1/installs/{install_id}/reprovision-sandbox
func (c *Client) ReprovisionSandbox(ctx context.Context, orgID, installID string) error {
	if installID == "" {
		return errors.New("install_id is required")
	}
	path := fmt.Sprintf("/v1/installs/%s/reprovision-sandbox", url.PathEscape(installID))
	c.logger.Info("triggering sandbox reprovision",
		zap.String("install_id", installID),
		zap.String("path", path))
	return c.do(ctx, http.MethodPost, orgID, path, nil)
}

// DeployComponent triggers an apply (plan_only=false) of a component on an
// install. Used to roll any drift back to the configured state.
//
//	POST /v1/installs/{install_id}/components/{component_id}/deploys
func (c *Client) DeployComponent(ctx context.Context, orgID, installID, componentID string) error {
	if installID == "" {
		return errors.New("install_id is required")
	}
	if componentID == "" {
		return errors.New("component_id is required")
	}
	path := fmt.Sprintf(
		"/v1/installs/%s/components/%s/deploys",
		url.PathEscape(installID),
		url.PathEscape(componentID),
	)
	body := map[string]any{
		// Force the apply phase — drift remediation that stops at plan
		// would defeat the entire point of this extension.
		"plan_only": false,
	}
	c.logger.Info("triggering component redeploy",
		zap.String("install_id", installID),
		zap.String("component_id", componentID),
		zap.String("path", path))
	return c.do(ctx, http.MethodPost, orgID, path, body)
}

func (c *Client) do(ctx context.Context, method, orgID, path string, body any) error {
	target := c.baseURL + path

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	effectiveOrg := strings.TrimSpace(orgID)
	if effectiveOrg == "" {
		effectiveOrg = c.orgID
	}
	if effectiveOrg != "" {
		req.Header.Set("X-Nuon-Org-ID", effectiveOrg)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf(
			"%s %s: status %d: %s",
			method,
			path,
			resp.StatusCode,
			strings.TrimSpace(string(respBody)),
		)
	}
	return nil
}
