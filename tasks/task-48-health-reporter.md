# Task 48: Automated Resource Plane Health Reporting

Status: Local implementation complete; paired Secret provisioning/rotation, live AWS deployment, and probe/alarms verification remain approval-gated. Priority: P2. Depends on: none.

## Goal

Turn the current operator-entered Health signal and persistence seam into an authenticated, observable Resource Plane report flow.

## Work

- Define the Reporter identity/protocol and narrow ingress (signed request or queue policy); do not accept arbitrary unauthenticated health updates.
- Add scheduled probes for the actual capabilities needed by placement (controller/queue/provider/EFS as appropriate), with bounded timeout and source timestamps.
- Call `PutAutomated` through a dedicated role; prevent machine reporter from changing lifecycle status or overwriting operator audit semantics.
- Add stale-report, repeated-unhealthy, probe-failure, and recovery metrics/alarms and a safe disable/drain runbook.
- Reconcile README claims with implemented reporter deployment and health history.

## Progress (2026-10-12)

- Implemented report-ID event history and a partial unique index per Resource Plane. Exact duplicate reports are idempotent; changed-content reuse, old/future timestamps, and out-of-order observations are rejected.
- Added an opt-in API route requiring route-bound Resource Plane HMAC-SHA256 authentication, a minimum 32-byte key resolver, bounded strict JSON, timestamp window, and transactional persistence. The resolver is injected; no secret material is stored in DSQL.
- Added a probe aggregation library with per-probe timeout, concurrency bound, sanitized failed-probe names, partial-failure degradation, all-failure unhealthy status, and healthy recovery; unit tests cover timeout/partial/recovery behavior. The Lambda composition probes command/result SQS queue metadata, the exact configured Lambda MicroVM image version (matching ARN/version, `SUCCESSFUL` build state, and `ACTIVE` launch status), the exact configured VPC egress connector (matching ARN and `ACTIVE` state), exact Workload controller function configuration (`Active`, no failed update), and exact Workspace EFS file system (matching ID, `available`, encrypted, at least one mount target). It does not fetch or log VPC configuration fields. Controller/EFS metadata and mount-target evidence are not proof of live queue processing, MicroVM launch, or a successful EFS mount from a Workspace.
- Added the HTTPS reporter client with signed timestamp/body, deterministic caller-supplied report ID for retry, a hard request timeout, redirect refusal, bounded response reading, and redacted transport/status errors.
- Added an opt-in scheduled ARM64 Lambda reporter (`rate(1 minute)`) that checks command/result queue metadata, one configured immutable MicroVM image version, one VPC egress connector, the exact controller function, and one Workspace EFS; reads its exact Plane Secret at runtime; sends signed reports; and emits sanitized CloudWatch EMF metrics. Its role receives resource-scoped read-only actions for those exact resources, including the Controller function ARN and EFS filesystem ARN.
- Wired opt-in Control Plane Secret-ARN map configuration, exact-ARN `GetSecretValue` IAM, API Gateway `NONE` route plus route throttling; no Secret values are Terraform variables. Empty map disables the route. Added a default-off Resource Plane reporter role constrained to its exact Secret, two queue ARNs, image, connector, Controller function ARN, EFS filesystem ARN, and retained log group, plus matching health alarms.
- Added a health metric contract for heartbeat, unhealthy state, probe failures, and reporter failures; tests cover normal, degraded, unhealthy, and ingress-outage metric sets.
- PostgreSQL integration coverage includes forged HMAC, successful report, duplicate report, changed-content conflict, stale/future and out-of-order timestamps, and legacy operator report behavior. Unit tests cover a Control Plane outage and confirm the response body is not leaked.
- Updated `docs/resource-plane-health.md` with the wire protocol and safe disable/drain runbook.
- Not complete: provider probes verify metadata/state only; they do not prove a MicroVM can launch/execute, connector network policy is safe, traffic follows it, a Workspace can mount EFS, or the controller successfully processes a queue message. Paired per-Plane HMAC Secret provisioning/rotation and live deployment/alarms need operator setup. Terraform is opt-in and statically validated only; no AWS plan/apply or Secret read was performed.

## Local verification

- `GOCACHE=/tmp/hako-go-cache go test -race ./internal/healthreport ./cmd/hako-health-reporter-lambda -count=1`
- `GOCACHE=/tmp/hako-go-cache go test ./...`
- `HAKO_TEST_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable' GOCACHE=/tmp/hako-go-cache go test -tags=integration ./internal/store/resourceplanehealth ./internal/api -count=1`
- Resource Plane Terraform `validate` and `fmt -check` passed; no AWS plan/apply was run.

## Acceptance criteria

- Tests cover forged identity, replay, stale/future timestamp, duplicate report, partial probe failure, recovery, and reporter outages.
- Scheduler behavior for unknown/fresh/stale healthy/degraded/unhealthy reports remains as documented in `docs/resource-plane-health.md`.
- Terraform grants only the required health update operation and creates alarm/log retention resources; validate/plan only.
