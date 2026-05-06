// Package payload models the public Nuon CloudEvents v1.0 webhook envelope
// emitted by ctl-api lifecycle hooks. Only the subset of fields required to
// detect drift events and route remediation is decoded; unknown fields are
// preserved by the JSON decoder but ignored.
package payload

import (
	"errors"
	"fmt"
	"strings"
)

// CloudEvent type identifiers emitted by ctl-api workflow / step lifecycle
// hooks.
const (
	CloudEventTypeWorkflow             = "com.nuon.workflow.lifecycle.v1"
	CloudEventTypeWorkflowStep         = "com.nuon.workflow_step.lifecycle.v1"
	CloudEventTypeWorkflowStepApproval = "com.nuon.workflow_step.approval.v1"
)

// SlugEventDriftDetected is the interests slug stamped onto webhook payloads
// that originate from a drift-detected signal (the dedicated event class
// emitted from inside drift_run / drift_run_reprovision_sandbox workflows
// when a plan-only check actually finds drift).
const SlugEventDriftDetected = "event:drift.detected"

// Workflow type values that originate drift-detected events. These map
// 1:1 onto the remediation strategy:
//   - WorkflowTypeDriftRun                  → component redeploy
//   - WorkflowTypeDriftRunReprovisionSandbox → sandbox reprovision
const (
	WorkflowTypeDriftRun                   = "drift_run"
	WorkflowTypeDriftRunReprovisionSandbox = "drift_run_reprovision_sandbox"
)

// CloudEvent is the CloudEvents v1.0 envelope used by ctl-api lifecycle
// webhooks. Only the fields the remediator needs are typed; the wire format
// has additional fields (subject, datacontenttype, etc.) that are decoded
// but unused.
type CloudEvent struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Type            string `json:"type"`
	Source          string `json:"source"`
	Time            string `json:"time,omitempty"`
	Subject         string `json:"subject,omitempty"`
	DataContentType string `json:"datacontenttype,omitempty"`

	// Nuon CloudEvents extension attributes. NuonOrgID is the source of
	// truth for which org's API to call.
	NuonOrgID      string `json:"nuonorgid,omitempty"`
	NuonKind       string `json:"nuonkind,omitempty"`
	NuonTransition string `json:"nuontransition,omitempty"`

	// Interests is the slug list produced by the ctl-api interests
	// classifier. The remediator filters on the presence of
	// SlugEventDriftDetected here.
	Interests []string `json:"interests,omitempty"`

	Data *LifecycleEventData `json:"data"`
}

// LifecycleEventData mirrors the public payload built by
// ctl-api/internal/pkg/queue/signal/hooks/webhook.go.
type LifecycleEventData struct {
	Kind       string         `json:"kind"`
	Transition string         `json:"transition"`
	OrgID      string         `json:"org_id,omitempty"`
	OrgName    string         `json:"org_name,omitempty"`
	Workflow   WorkflowRef    `json:"workflow"`
	Step       *WorkflowStep  `json:"step,omitempty"`
	Outcome    *Outcome       `json:"outcome,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// WorkflowRef identifies the parent workflow. For install workflows
// OwnerType == "installs" and OwnerID is the install id.
type WorkflowRef struct {
	ID        string `json:"id"`
	Type      string `json:"type,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
	OwnerType string `json:"owner_type,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
}

// WorkflowStep is the step-level payload. For drift events the relevant
// fields are ComponentID and SandboxID — exactly one is populated for
// drift-detected events that target a single resource.
type WorkflowStep struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Idx           int    `json:"idx"`
	TargetType    string `json:"target_type,omitempty"`
	TargetID      string `json:"target_id,omitempty"`
	ComponentID   string `json:"component_id,omitempty"`
	ComponentName string `json:"component_name,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty"`
	ExecutionType string `json:"execution_type,omitempty"`
}

// Outcome carries the terminal status of a lifecycle event.
type Outcome struct {
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// Validate performs the minimum sanity checks the remediator relies on:
// CloudEvents v1.0 envelope, non-empty id/type/source, decoded data block.
// Drift-specific filtering is intentionally NOT enforced here — Validate
// runs on every payload we receive; the drift filter lives in IsDrift().
func (ce *CloudEvent) Validate() error {
	if ce == nil {
		return errors.New("payload is nil")
	}
	if ce.SpecVersion != "1.0" {
		return fmt.Errorf("unsupported specversion %q", ce.SpecVersion)
	}
	if strings.TrimSpace(ce.ID) == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(ce.Type) == "" {
		return errors.New("type is required")
	}
	if strings.TrimSpace(ce.Source) == "" {
		return errors.New("source is required")
	}
	if ce.Data == nil {
		return errors.New("data is required")
	}
	return nil
}

// IsDrift returns true when the event's interests slug list contains
// SlugEventDriftDetected. ctl-api stamps this slug onto every event that
// originates from a drift-detected signal, so a single membership check is
// sufficient regardless of the surrounding workflow type or transition.
func (ce *CloudEvent) IsDrift() bool {
	if ce == nil {
		return false
	}
	for _, slug := range ce.Interests {
		if slug == SlugEventDriftDetected {
			return true
		}
	}
	return false
}

// DriftTarget is the resolved remediation directive: which install, which
// resource, and which strategy applies.
type DriftTarget struct {
	Kind        DriftKind
	OrgID       string
	InstallID   string
	ComponentID string
	SandboxID   string
}

// DriftKind enumerates the supported remediation strategies.
type DriftKind string

const (
	DriftKindComponent DriftKind = "component"
	DriftKindSandbox   DriftKind = "sandbox"
)

// ResolveDriftTarget extracts the remediation target from a drift-detected
// event. It returns ok=false (with a human-readable reason) when the event
// can't be remediated — most commonly because the install id is missing or
// neither component_id nor sandbox_id is present on the step.
//
// Precedence rules:
//   - WorkflowType == drift_run_reprovision_sandbox → sandbox remediation
//     (the workflow operates on the entire sandbox; component_id on the
//     step is incidental).
//   - SandboxID present                              → sandbox remediation.
//   - ComponentID present                            → component remediation.
//   - Otherwise                                      → not remediable.
func (ce *CloudEvent) ResolveDriftTarget() (DriftTarget, string, bool) {
	if ce == nil || ce.Data == nil {
		return DriftTarget{}, "payload is empty", false
	}

	orgID := strings.TrimSpace(ce.NuonOrgID)
	if orgID == "" {
		orgID = strings.TrimSpace(ce.Data.OrgID)
	}

	installID := installIDFromWorkflow(ce.Data.Workflow)
	if installID == "" {
		return DriftTarget{}, "install_id is unresolved (workflow.owner_type != installs)", false
	}

	target := DriftTarget{OrgID: orgID, InstallID: installID}

	if ce.Data.Workflow.Type == WorkflowTypeDriftRunReprovisionSandbox {
		target.Kind = DriftKindSandbox
		if ce.Data.Step != nil {
			target.SandboxID = ce.Data.Step.SandboxID
		}
		return target, "", true
	}

	if ce.Data.Step != nil {
		switch {
		case ce.Data.Step.SandboxID != "":
			target.Kind = DriftKindSandbox
			target.SandboxID = ce.Data.Step.SandboxID
			return target, "", true
		case ce.Data.Step.ComponentID != "":
			target.Kind = DriftKindComponent
			target.ComponentID = ce.Data.Step.ComponentID
			return target, "", true
		}
	}

	// Workflow-level drift_run with no step block — fall back to component
	// remediation against an unspecified component (the API requires a
	// component_id, so this case fails fast with a helpful message).
	if ce.Data.Workflow.Type == WorkflowTypeDriftRun {
		return DriftTarget{}, "drift_run event missing step.component_id", false
	}

	return DriftTarget{}, "neither step.component_id nor step.sandbox_id is set", false
}

// installIDFromWorkflow returns the install id when the workflow's owner is
// an install. Returns "" otherwise.
func installIDFromWorkflow(w WorkflowRef) string {
	if w.OwnerType == "installs" {
		return strings.TrimSpace(w.OwnerID)
	}
	return ""
}
