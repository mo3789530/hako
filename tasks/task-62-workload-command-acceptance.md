# Task 62: Durable Resource Plane Command Acceptance

Status: Complete (library; queue wiring and MicroVM launch remain pending). Priority: P0. Depends on: Task 39 journal foundations.

## Result

`internal/workloadcontroller` provides strict versioned dispatch decoding and a Resource Plane-bound Acceptor. It conditionally creates an attempt journal, reloads conflicts, checks every immutable identity field and ETag, and preserves existing cancellation/execution/cleanup checkpoints. Invalid input and storage failures do not report success. No DSQL connection or runtime invocation is introduced.

## Acceptance criteria

- [x] Reject unknown execution parameters, invalid identity/schema, wrong Resource Plane, trailing JSON, and oversized envelopes.
- [x] Concurrent duplicate delivery creates one durable journal; existing cancel and cleanup states survive retries.
- [x] A mismatched journal checkpoint or failed persistence never becomes acceptance success.
- [x] Dependency-injected tests run without AWS resources or secrets.

## Integration boundary

Acceptance alone does not execute a Job. Do not deploy a queue consumer that acknowledges here until Task 63 supplies recovery for accepted journals. MicroVM launch, fencing after launch, results, cancellation, and physical cleanup remain incomplete.
