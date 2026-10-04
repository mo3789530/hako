# Task 48: Automated Resource Plane Health Reporting

Status: Ready. Priority: P2. Depends on: none.

## Goal

Turn the current operator-entered Health signal and persistence seam into an authenticated, observable Resource Plane report flow.

## Work

- Define the Reporter identity/protocol and narrow ingress (signed request or queue policy); do not accept arbitrary unauthenticated health updates.
- Add scheduled probes for the actual capabilities needed by placement (controller/queue/provider/EFS as appropriate), with bounded timeout and source timestamps.
- Call `PutAutomated` through a dedicated role; prevent machine reporter from changing lifecycle status or overwriting operator audit semantics.
- Add stale-report, repeated-unhealthy, probe-failure, and recovery metrics/alarms and a safe disable/drain runbook.
- Reconcile README claims with implemented reporter deployment and health history.

## Acceptance criteria

- Tests cover forged identity, replay, stale/future timestamp, duplicate report, partial probe failure, recovery, and reporter outages.
- Scheduler behavior for unknown/fresh/stale healthy/degraded/unhealthy reports remains as documented in `docs/resource-plane-health.md`.
- Terraform grants only the required health update operation and creates alarm/log retention resources; validate/plan only.
