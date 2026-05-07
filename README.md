# nuon-ext-drift-remediator

Webhook receiver extension that auto-remediates Nuon drift-detected events.

When ctl-api emits a CloudEvent whose `interests` slug list contains
`event:drift.detected`, this extension calls the appropriate Nuon API to roll
the resource back to its declared state — regardless of the configured plan
strategy:

- **sandbox drift** → `POST /v1/installs/{install_id}/reprovision-sandbox`
- **component drift** → `POST /v1/installs/{install_id}/components/{component_id}/deploys` (`plan_only: false`)

There is no plan-only safety net. The extension is meant for environments
where any detected drift should be reverted automatically.

## Install

```bash
nuon ext install nuonco/nuon-ext-drift-remediator
```

## Run

```bash
nuon drift-remediator serve \
  --addr :8788 \
  --api-token "$NUON_API_TOKEN" \
  --secret "$NUON_WEBHOOK_SECRET"
```

By default, the server exposes:

1. `POST /webhooks/drift` for drift-detected webhooks.
2. `GET  /healthz` for health checks.

## Configure ctl-api

Create an organization webhook in ctl-api with:

- **URL**: `http://your-host:8788/webhooks/drift`
- **Secret**: matches `--secret` / `NUON_WEBHOOK_SECRET`
- **Interests**: must include `event:drift.detected`. The simplest setup is
  `drift_detected: true` on every resource you care about (components,
  sandboxes); ctl-api stamps `event:drift.detected` onto those events.

The extension also accepts events that carry other interest slugs alongside
the drift slug — non-drift events are acked with `200 OK` and `drift: false`
without taking action, so it is safe to point a broader webhook at it.

## Authentication

| Where        | What                                              |
| ------------ | ------------------------------------------------- |
| API token    | `--api-token` flag or `NUON_API_TOKEN` env var.   |
| Org ID       | `--org-id` flag or `NUON_ORG_ID` env var, or      |
|              | falls back to `nuonorgid` on the incoming event.  |
| HMAC secret  | `--secret` flag or `NUON_WEBHOOK_SECRET` env var. |

If `--api-token` is unset and `--dry-run` is not enabled, the server logs a
warning at start-up and remediation calls fail with `401`.

## Dry-run mode

```bash
nuon drift-remediator serve --dry-run
```

Logs the resolved remediation target (`install_id`, `component_id` or
`sandbox_id`, action) for every drift event but skips the API call. Useful
for validating the webhook payloads ctl-api is producing before granting
write credentials.

## Webhook payload contract

The receiver expects a CloudEvents v1.0 envelope of type
`com.nuon.workflow_step.lifecycle.v1` (or `com.nuon.workflow.lifecycle.v1`)
with the standard Nuon extension attributes:

- `interests` array containing `event:drift.detected`
- `data.workflow.owner_type == "installs"` so an install id can be resolved
- `data.step.component_id` (component drift) or `data.step.sandbox_id`
  (sandbox drift), or
- `data.workflow.type == "drift_run_reprovision_sandbox"` for sandbox drift

When the install id can't be resolved or both component_id and sandbox_id are
missing, the receiver returns `200 OK` with a non-empty `reason` so ctl-api
stops retrying — the event is logged as a warning for operator inspection.

When the API call itself fails the receiver returns `502 Bad Gateway`, which
makes ctl-api retry the webhook (and remediation) per its normal
delivery-failure policy.

## Ack response

```json
{
  "ok": true,
  "received_at": "2026-05-06T00:00:00Z",
  "event_id": "5f1c5b0e-2c8e-4f8b-9b4d-2f6c1f3a4d5e",
  "drift": true,
  "remediated": true,
  "action": "redeploy-component"
}
```

| Field        | Meaning                                                     |
| ------------ | ----------------------------------------------------------- |
| `drift`      | Event carried `event:drift.detected` in `interests`.        |
| `remediated` | API call dispatched successfully (false in dry-run mode).   |
| `action`     | `reprovision-sandbox` or `redeploy-component`.              |
| `reason`     | Non-empty when the event was acked without remediation.     |

## Build

```bash
make build       # produces ./nuon-ext-drift-remediator
make test        # runs go test ./...
```

## Local development

```bash
# Build + install from this directory (creates a symlink in ~/.config/nuon/extensions)
go build -o nuon-ext-drift-remediator .
NUON_PREVIEW=true nuon ext install ./.

# Run via the proxy command
NUON_PREVIEW=true nuon drift-remediator serve --pretty-logs --dry-run
```
