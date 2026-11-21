# Task 64: Workload Cancellation and Physical Cleanup

Status: Implemented locally (CP Outbox, RP library, authenticated Agent and Worker composition). Default-off deployment and live AWS worker/agent behavior remain pending Task 40/45. Priority: P0. Depends on: 62, 63.

## Result

- [x] CP desired cancellation and current-attempt Outbox share one transaction. Duplicate requests do not create new cancel events; no-attempt cancellation stays scheduler-owned.
- [x] Dispatcher routes `workload_attempt.cancel` to the pinned RP. `HandleEvent` distinguishes it from dispatch and rejects unknown event types.
- [x] RP persists tombstone before stopping. Cancel-before-launch and cancel/launch races never clear the tombstone.
- [x] Unknown RunMicrovm response recovers the allocation using frozen ClientToken/parameters. Cleanup refreshes the runtime binding checkpoint before CAS.
- [x] Frozen specification metadata survives repository resolver revocation; cancellation recovery does not request GitHub credentials.
- [x] Failed physical cleanup remains retryable; confirmation requires observed termination, not just a successful stop request.
- [x] Replacement waits for exact predecessor generation cleanup confirmation.
- [x] PostgreSQL tests verify cancellation Outbox rollback and idempotency, and cancel-before-dispatch through RP to CP terminal result.

The transport, bounded recovery handler, and authenticated Agent are locally composed; Terraform models their default-off deployment. AWS deployment, live recovery, and actual MicroVM race/isolation evidence remain pending. Feature flags remain off. See [controller design](../docs/lambda-microvm-workloads.md).
