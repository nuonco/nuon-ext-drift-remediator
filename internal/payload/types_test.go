package payload

import "testing"

func validDriftEvent() *CloudEvent {
	return &CloudEvent{
		SpecVersion: "1.0",
		ID:          "evt_123",
		Type:        CloudEventTypeWorkflowStep,
		Source:      "//nuon.co/ctl-api",
		NuonOrgID:   "org_123",
		Interests:   []string{"resource:components", "op:components.drift", SlugEventDriftDetected},
		Data: &LifecycleEventData{
			Kind:       "workflow_step",
			Transition: "succeeded",
			OrgID:      "org_123",
			Workflow: WorkflowRef{
				ID:        "wf_123",
				Type:      WorkflowTypeDriftRun,
				OwnerID:   "ins_123",
				OwnerType: "installs",
			},
			Step: &WorkflowStep{
				ID:          "step_123",
				ComponentID: "cmp_123",
			},
		},
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*CloudEvent)
		wantErr bool
	}{
		{name: "valid", mutate: func(*CloudEvent) {}},
		{name: "wrong specversion", mutate: func(ce *CloudEvent) { ce.SpecVersion = "0.3" }, wantErr: true},
		{name: "missing id", mutate: func(ce *CloudEvent) { ce.ID = "" }, wantErr: true},
		{name: "missing type", mutate: func(ce *CloudEvent) { ce.Type = "" }, wantErr: true},
		{name: "missing source", mutate: func(ce *CloudEvent) { ce.Source = "" }, wantErr: true},
		{name: "missing data", mutate: func(ce *CloudEvent) { ce.Data = nil }, wantErr: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := validDriftEvent()
			tc.mutate(ce)
			err := ce.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	t.Run("nil receiver", func(t *testing.T) {
		t.Parallel()
		var ce *CloudEvent
		if err := ce.Validate(); err == nil {
			t.Fatalf("expected error for nil")
		}
	})
}

func TestIsDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		interests []string
		want      bool
	}{
		{name: "drift slug present", interests: []string{"resource:components", SlugEventDriftDetected}, want: true},
		{name: "drift slug absent", interests: []string{"resource:components", "outcome:successes"}, want: false},
		{name: "empty slugs", interests: nil, want: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := validDriftEvent()
			ce.Interests = tc.interests
			if got := ce.IsDrift(); got != tc.want {
				t.Fatalf("IsDrift = %v want %v", got, tc.want)
			}
		})
	}

	t.Run("nil receiver", func(t *testing.T) {
		t.Parallel()
		var ce *CloudEvent
		if ce.IsDrift() {
			t.Fatalf("nil should not be drift")
		}
	})
}

func TestResolveDriftTarget(t *testing.T) {
	t.Parallel()

	t.Run("component drift via drift_run", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		target, reason, ok := ce.ResolveDriftTarget()
		if !ok {
			t.Fatalf("expected ok, got reason=%q", reason)
		}
		if target.Kind != DriftKindComponent {
			t.Fatalf("kind = %q, want component", target.Kind)
		}
		if target.InstallID != "ins_123" || target.ComponentID != "cmp_123" {
			t.Fatalf("unexpected target: %+v", target)
		}
		if target.OrgID != "org_123" {
			t.Fatalf("org id = %q want org_123", target.OrgID)
		}
	})

	t.Run("sandbox drift via drift_run_reprovision_sandbox", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		ce.Data.Workflow.Type = WorkflowTypeDriftRunReprovisionSandbox
		ce.Data.Step.ComponentID = ""
		ce.Data.Step.SandboxID = "sbx_123"
		target, reason, ok := ce.ResolveDriftTarget()
		if !ok {
			t.Fatalf("expected ok, got reason=%q", reason)
		}
		if target.Kind != DriftKindSandbox {
			t.Fatalf("kind = %q, want sandbox", target.Kind)
		}
		if target.SandboxID != "sbx_123" || target.InstallID != "ins_123" {
			t.Fatalf("unexpected target: %+v", target)
		}
	})

	t.Run("sandbox precedence for drift_run_reprovision_sandbox even when component_id present", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		ce.Data.Workflow.Type = WorkflowTypeDriftRunReprovisionSandbox
		// step has ComponentID from validDriftEvent fixture
		target, _, ok := ce.ResolveDriftTarget()
		if !ok {
			t.Fatalf("expected ok")
		}
		if target.Kind != DriftKindSandbox {
			t.Fatalf("kind = %q, want sandbox (workflow type wins)", target.Kind)
		}
	})

	t.Run("missing install id", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		ce.Data.Workflow.OwnerType = "orgs"
		_, reason, ok := ce.ResolveDriftTarget()
		if ok {
			t.Fatalf("expected not ok")
		}
		if reason == "" {
			t.Fatalf("expected reason")
		}
	})

	t.Run("missing step", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		ce.Data.Step = nil
		_, _, ok := ce.ResolveDriftTarget()
		if ok {
			t.Fatalf("expected not ok")
		}
	})

	t.Run("step without ids", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		ce.Data.Step.ComponentID = ""
		ce.Data.Step.SandboxID = ""
		_, _, ok := ce.ResolveDriftTarget()
		if ok {
			t.Fatalf("expected not ok")
		}
	})

	t.Run("falls back to data.org_id when nuonorgid missing", func(t *testing.T) {
		t.Parallel()
		ce := validDriftEvent()
		ce.NuonOrgID = ""
		target, _, ok := ce.ResolveDriftTarget()
		if !ok {
			t.Fatalf("expected ok")
		}
		if target.OrgID != "org_123" {
			t.Fatalf("org id = %q", target.OrgID)
		}
	})
}
