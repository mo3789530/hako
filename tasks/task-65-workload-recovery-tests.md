# Task 65: Workload Recovery and Local End-to-End Tests

Status: Implemented (local fake/provider-stub + PostgreSQL verification); AWS PoC remains Task 40. Priority: P0. Depends on: 63, 64.

## Coverage

- [x] Duplicate dispatch, stale generation, specification drift, failed checkpoint before/after launch, unknown provider response, and same-token recovery.
- [x] Cancel before/during launch, durable stop fence, cleanup failure/retry, and predecessor cleanup gating.
- [x] 24-hour fake-clock CP deadline and eight-hour AWS adapter capability restriction.
- [x] Result collection failure, cross-Tenant/attempt artifacts, unsafe metadata, immutable result payload, send failure, and crash after send before checkpoint.
- [x] SDK stubs verify fixed RunMicrovm parameters, stored binding reuse, readiness gate, expected endpoint/role, VPC connector requirement, and observed termination.
- [x] PostgreSQL verifies RP result -> CP terminal/artifact metadata, applied -> duplicate replay after lost acknowledgement, and transactional cancel Outbox -> RP fence -> CP cancellation.

These tests do not claim actual AWS MicroVM isolation, real container execution, full GitHub Factory E2E (Task 42), or deployed recovery worker readiness. Test commands and remaining production requirements are documented in [Lambda MicroVM Workloads](../docs/lambda-microvm-workloads.md).
