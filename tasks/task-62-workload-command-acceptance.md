# Task 62: Durable Resource Plane Command Acceptance

Status: Complete (acceptance library). Task 40 now locally composes the SQS Worker, Lambda MicroVM runtime, Agent transport, results, and recovery; its Terraform deployment remains default-off and unapplied. Priority: P0. Depends on: Task 39 journal foundations.

## Result

`internal/workloadcontroller` provides strict versioned dispatch decoding and a Resource Plane-bound Acceptor. It conditionally creates an attempt journal, reloads conflicts, checks every immutable identity field and ETag, and preserves existing cancellation/execution/cleanup checkpoints. Invalid input and storage failures do not report success. No DSQL connection or runtime invocation is introduced.

## Acceptance criteria

- [x] Reject unknown execution parameters, invalid identity/schema, wrong Resource Plane, trailing JSON, and oversized envelopes.
- [x] Concurrent duplicate delivery creates one durable journal; existing cancel and cleanup states survive retries.
- [x] A mismatched journal checkpoint or failed persistence never becomes acceptance success.
- [x] Dependency-injected tests run without AWS resources or secrets.

## Integration boundary

Acceptance alone does not execute a Job. The queue consumer must only acknowledge after the durable journal checkpoint and retry/recovery path. Tasks 40, 63, and 64 now locally compose launch, fencing, results, cancellation, and physical cleanup; AWS deployment and live MicroVM behavior remain unverified.
