package nuonapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nuonco/nuon-ext-drift-remediator/internal/payload"
)

type capturedRequest struct {
	method string
	path   string
	auth   string
	orgID  string
	body   map[string]any
}

func newTestServer(t *testing.T, status int) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.orgID = r.Header.Get("X-Nuon-Org-ID")
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			_ = json.Unmarshal(body, &captured.body)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func TestReprovisionSandbox(t *testing.T) {
	t.Parallel()

	srv, captured := newTestServer(t, http.StatusCreated)
	c := New(Config{BaseURL: srv.URL, APIToken: "token-abc", OrgID: "org_default"})

	if err := c.ReprovisionSandbox(context.Background(), "org_xyz", "ins_123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.method != http.MethodPost {
		t.Fatalf("method = %q want POST", captured.method)
	}
	if captured.path != "/v1/installs/ins_123/reprovision-sandbox" {
		t.Fatalf("path = %q", captured.path)
	}
	if captured.auth != "Bearer token-abc" {
		t.Fatalf("authorization = %q", captured.auth)
	}
	if captured.orgID != "org_xyz" {
		t.Fatalf("org id = %q want event override", captured.orgID)
	}
}

func TestReprovisionSandboxFallsBackToConfiguredOrg(t *testing.T) {
	t.Parallel()

	srv, captured := newTestServer(t, http.StatusCreated)
	c := New(Config{BaseURL: srv.URL, APIToken: "token", OrgID: "org_default"})

	if err := c.ReprovisionSandbox(context.Background(), "", "ins_123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.orgID != "org_default" {
		t.Fatalf("org id = %q want fallback to configured org", captured.orgID)
	}
}

func TestDeployComponent(t *testing.T) {
	t.Parallel()

	srv, captured := newTestServer(t, http.StatusCreated)
	c := New(Config{BaseURL: srv.URL, APIToken: "token"})

	if err := c.DeployComponent(context.Background(), "org_xyz", "ins_123", "cmp_123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.path != "/v1/installs/ins_123/components/cmp_123/deploys" {
		t.Fatalf("path = %q", captured.path)
	}
	if planOnly, ok := captured.body["plan_only"].(bool); !ok || planOnly {
		t.Fatalf("expected plan_only=false in body, got %#v", captured.body)
	}
}

func TestDeployComponentMissingIDs(t *testing.T) {
	t.Parallel()

	c := New(Config{BaseURL: "http://example.invalid", APIToken: "x"})
	if err := c.DeployComponent(context.Background(), "org", "", "cmp"); err == nil {
		t.Fatalf("expected error for missing install_id")
	}
	if err := c.DeployComponent(context.Background(), "org", "ins", ""); err == nil {
		t.Fatalf("expected error for missing component_id")
	}
}

func TestRemediateRoutes(t *testing.T) {
	t.Parallel()

	srv, captured := newTestServer(t, http.StatusCreated)
	c := New(Config{BaseURL: srv.URL, APIToken: "token"})

	cases := []struct {
		name   string
		target payload.DriftTarget
		path   string
	}{
		{
			name: "sandbox",
			target: payload.DriftTarget{
				Kind:      payload.DriftKindSandbox,
				OrgID:     "org_1",
				InstallID: "ins_1",
			},
			path: "/v1/installs/ins_1/reprovision-sandbox",
		},
		{
			name: "component",
			target: payload.DriftTarget{
				Kind:        payload.DriftKindComponent,
				OrgID:       "org_1",
				InstallID:   "ins_1",
				ComponentID: "cmp_1",
			},
			path: "/v1/installs/ins_1/components/cmp_1/deploys",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Remediate(context.Background(), tc.target); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if captured.path != tc.path {
				t.Fatalf("path = %q want %q", captured.path, tc.path)
			}
		})
	}
}

func TestNon2xxIsError(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServer(t, http.StatusUnauthorized)
	c := New(Config{BaseURL: srv.URL, APIToken: "bad"})

	err := c.ReprovisionSandbox(context.Background(), "org", "ins_123")
	if err == nil {
		t.Fatalf("expected error")
	}
}
