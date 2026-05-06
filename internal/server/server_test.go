package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/nuonco/nuon-ext-drift-remediator/internal/payload"
)

const testWebhookPath = "/webhooks/drift"

type stubRemediator struct {
	mu      sync.Mutex
	called  int
	last    payload.DriftTarget
	failure error
}

func (s *stubRemediator) Remediate(_ context.Context, target payload.DriftTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called++
	s.last = target
	return s.failure
}

func (s *stubRemediator) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

func driftEvent() payload.CloudEvent {
	return payload.CloudEvent{
		SpecVersion: "1.0",
		ID:          "evt_drift_1",
		Type:        payload.CloudEventTypeWorkflowStep,
		Source:      "//nuon.co/ctl-api",
		NuonOrgID:   "org_123",
		Interests:   []string{"resource:components", payload.SlugEventDriftDetected},
		Data: &payload.LifecycleEventData{
			Kind:       "workflow_step",
			Transition: "succeeded",
			OrgID:      "org_123",
			Workflow: payload.WorkflowRef{
				ID:        "wf_1",
				Type:      payload.WorkflowTypeDriftRun,
				OwnerID:   "ins_1",
				OwnerType: "installs",
			},
			Step: &payload.WorkflowStep{
				ID:          "step_1",
				ComponentID: "cmp_1",
			},
		},
	}
}

func newServer(t *testing.T, secret string, dryRun bool) (*Server, *stubRemediator) {
	t.Helper()
	stub := &stubRemediator{}
	srv := New(Config{
		Logger:         zap.NewNop(),
		WebhookPath:    testWebhookPath,
		HealthPath:     "/healthz",
		ExpectedSecret: secret,
		Remediator:     stub,
		DryRun:         dryRun,
	})
	return srv, stub
}

func postEvent(t *testing.T, srv *Server, ce payload.CloudEvent, secret string) (*httptest.ResponseRecorder, ackResponse) {
	t.Helper()
	body, err := json.Marshal(ce)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, testWebhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json; charset=utf-8")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Nuon-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var ack ackResponse
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &ack)
	}
	return rec, ack
}

func TestComponentDriftTriggersRedeploy(t *testing.T) {
	t.Parallel()

	srv, stub := newServer(t, "", false)
	rec, ack := postEvent(t, srv, driftEvent(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !ack.Drift || !ack.Remediated {
		t.Fatalf("ack = %#v", ack)
	}
	if ack.Action != "redeploy-component" {
		t.Fatalf("action = %q", ack.Action)
	}
	if stub.Calls() != 1 {
		t.Fatalf("expected 1 remediation call, got %d", stub.Calls())
	}
	if stub.last.Kind != payload.DriftKindComponent || stub.last.ComponentID != "cmp_1" {
		t.Fatalf("unexpected target: %+v", stub.last)
	}
}

func TestSandboxDriftTriggersReprovision(t *testing.T) {
	t.Parallel()

	srv, stub := newServer(t, "", false)
	ce := driftEvent()
	ce.Data.Workflow.Type = payload.WorkflowTypeDriftRunReprovisionSandbox
	ce.Data.Step.ComponentID = ""
	ce.Data.Step.SandboxID = "sbx_1"

	rec, ack := postEvent(t, srv, ce, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if ack.Action != "reprovision-sandbox" {
		t.Fatalf("action = %q", ack.Action)
	}
	if stub.Calls() != 1 {
		t.Fatalf("calls = %d", stub.Calls())
	}
	if stub.last.Kind != payload.DriftKindSandbox || stub.last.SandboxID != "sbx_1" {
		t.Fatalf("unexpected target: %+v", stub.last)
	}
}

func TestNonDriftEventIgnored(t *testing.T) {
	t.Parallel()

	srv, stub := newServer(t, "", false)
	ce := driftEvent()
	ce.Interests = []string{"resource:components", "outcome:successes"}

	rec, ack := postEvent(t, srv, ce, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ack.Drift {
		t.Fatalf("expected drift=false")
	}
	if ack.Remediated {
		t.Fatalf("expected remediated=false")
	}
	if stub.Calls() != 0 {
		t.Fatalf("expected zero calls, got %d", stub.Calls())
	}
}

func TestDryRunSkipsRemediation(t *testing.T) {
	t.Parallel()

	srv, stub := newServer(t, "", true)
	rec, ack := postEvent(t, srv, driftEvent(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !ack.Drift || ack.Remediated {
		t.Fatalf("ack = %#v want drift=true remediated=false", ack)
	}
	if ack.Reason != "dry-run" {
		t.Fatalf("reason = %q", ack.Reason)
	}
	if stub.Calls() != 0 {
		t.Fatalf("expected zero calls in dry-run, got %d", stub.Calls())
	}
}

func TestRemediationFailureReturns502(t *testing.T) {
	t.Parallel()

	srv, stub := newServer(t, "", false)
	stub.failure = errors.New("boom")

	rec, _ := postEvent(t, srv, driftEvent(), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502", rec.Code)
	}
}

func TestUnresolvableTargetReturns200WithReason(t *testing.T) {
	t.Parallel()

	srv, stub := newServer(t, "", false)
	ce := driftEvent()
	ce.Data.Step = nil

	rec, ack := postEvent(t, srv, ce, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (we want 200 so ctl-api stops retrying)", rec.Code)
	}
	if !ack.Drift {
		t.Fatalf("expected drift=true")
	}
	if ack.Remediated {
		t.Fatalf("expected remediated=false")
	}
	if stub.Calls() != 0 {
		t.Fatalf("calls = %d", stub.Calls())
	}
}

func TestSignatureValidation(t *testing.T) {
	t.Parallel()

	const secret = "shh"
	srv, _ := newServer(t, secret, false)

	t.Run("missing signature", func(t *testing.T) {
		rec, _ := postEvent(t, srv, driftEvent(), "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("valid signature", func(t *testing.T) {
		rec, _ := postEvent(t, srv, driftEvent(), secret)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("wrong signature", func(t *testing.T) {
		body, _ := json.Marshal(driftEvent())
		req := httptest.NewRequest(http.MethodPost, testWebhookPath, bytes.NewReader(body))
		req.Header.Set("X-Nuon-Signature", "deadbeef")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestInvalidJSON(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, "", false)
	req := httptest.NewRequest(http.MethodPost, testWebhookPath, bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, "", false)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}
